package dalgo2postgrest

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

const (
	maxFilterDepth       = 8
	maxFilterConditions  = 64
	maxOrderFields       = 16
	maxMembershipValues  = 100
	maxProjectionColumns = 256
	maxQueryBytes        = 32 << 10
	maxScalarBytes       = 4096
)

var reJSONNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// reservedFilterNames are query parameter names that PostgREST parses as
// controls rather than filters. A filter keyed by one of them would be dropped
// before the query is built (for example, a column named "limit" would become
// the page size). PostgREST's official identifier quoting marks such a key as
// a field name instead, so the constraint is kept.
var reservedFilterNames = map[string]struct{}{
	"select":      {},
	"columns":     {},
	"on_conflict": {},
	"order":       {},
	"limit":       {},
	"offset":      {},
	"and":         {},
	"or":          {},
}

// postgrestFieldKey renders a field name used as a PostgREST filter parameter
// key, including nested filter fields and the equality predicate of point
// reads. Field names are restricted to simple SQL identifiers, so a reserved
// name only needs surrounding double quotes and no escaping.
func postgrestFieldKey(name string) string {
	if _, reserved := reservedFilterNames[name]; reserved {
		return `"` + name + `"`
	}
	return name
}

type queryPlan struct {
	collection string
	params     url.Values
	limit      int
	offset     int
	pageSize   int
	// projected is nil when the result columns are derived from the response
	// (a wildcard read), and holds the requested output order otherwise.
	projected []string
	template  func() record.Record
}

func makePlan(query dal.Query, cfg config) (queryPlan, error) {
	if query == nil {
		return queryPlan{}, notSupported("a query is required")
	}
	q, ok := query.(dal.StructuredQuery)
	if !ok {
		return queryPlan{}, notSupported("native text queries; only structured relation reads are supported")
	}
	if dal.HasAggregation(q) || len(q.GroupBy()) != 0 || q.Having() != nil || dal.HasSubquery(q) {
		return queryPlan{}, notSupported("aggregates, grouping, having and subqueries")
	}
	if q.StartFrom() != "" || q.StartAfter() != "" {
		return queryPlan{}, notSupported("query cursors; use bounded offset/limit paging")
	}
	from := q.From()
	if from == nil || from.Base() == nil {
		return queryPlan{}, notSupported("a top-level relation is required")
	}
	if len(from.Joins()) != 0 {
		return queryPlan{}, notSupported("joins; PostgREST relations are read one at a time")
	}
	base, ok := collectionRefOf(from.Base())
	if !ok {
		return queryPlan{}, notSupported("only a single exposed relation can be queried")
	}
	if base.Parent() != nil {
		return queryPlan{}, notSupported("nested relations")
	}
	if base.Database() != "" {
		return queryPlan{}, notSupported("named databases; PostgREST routes one exposed schema profile")
	}
	if base.Schema() != "" && base.Schema() != cfg.schema {
		return queryPlan{}, notSupported("a relation schema that differs from Config.Schema")
	}
	if base.ScanLimit() != 0 || len(base.ScanOrders()) != 0 {
		return queryPlan{}, notSupported("scan limits and scan orders; they are meaningful only inside joins")
	}
	if !validIdentifier(base.Name()) {
		return queryPlan{}, notSupported("relation names must be simple SQL identifiers")
	}
	baseName := base.Name()
	baseAlias := base.Alias()

	limit := q.Limit()
	if limit <= 0 {
		return queryPlan{}, notSupported("unbounded reads; supply a positive limit")
	}
	if limit > cfg.maxRows {
		return queryPlan{}, fmt.Errorf("%w: query limit %d exceeds the configured maximum of %d rows", ErrBudgetExceeded, limit, cfg.maxRows)
	}
	offset := q.Offset()
	if offset < 0 {
		return queryPlan{}, notSupported("negative offsets")
	}
	if offset > cfg.maxOffset {
		return queryPlan{}, fmt.Errorf("%w: query offset %d exceeds the configured maximum of %d", ErrBudgetExceeded, offset, cfg.maxOffset)
	}
	pageSize := cfg.maxPageSize
	if limit < pageSize {
		pageSize = limit
	}
	pages := (limit + pageSize - 1) / pageSize
	if pages > cfg.maxRequests {
		return queryPlan{}, fmt.Errorf("%w: %d pages of at most %d rows exceed the request budget of %d", ErrBudgetExceeded, pages, pageSize, cfg.maxRequests)
	}

	params := url.Values{}
	projected, _, err := addProjection(params, q, cfg.idColumn, baseAlias, baseName)
	if err != nil {
		return queryPlan{}, err
	}
	if err = addOrder(params, q, cfg.idColumn, baseAlias, baseName); err != nil {
		return queryPlan{}, err
	}
	builder := filterBuilder{
		params:    params,
		baseAlias: baseAlias,
		baseName:  baseName,
	}
	if err = builder.add(q.Where(), 0); err != nil {
		return queryPlan{}, err
	}
	if bytes := len(params.Encode()); bytes > maxQueryBytes {
		return queryPlan{}, notSupported("query parameters longer than 32 KiB")
	}

	return queryPlan{
		collection: baseName,
		params:     params,
		limit:      limit,
		offset:     offset,
		pageSize:   pageSize,
		projected:  projected,
		template:   q.IntoRecord,
	}, nil
}

func collectionRefOf(source dal.RecordsetSource) (dal.CollectionRef, bool) {
	switch ref := source.(type) {
	case dal.CollectionRef:
		return ref, true
	case *dal.CollectionRef:
		if ref == nil {
			return dal.CollectionRef{}, false
		}
		return *ref, true
	default:
		return dal.CollectionRef{}, false
	}
}

func addProjection(params url.Values, q dal.StructuredQuery, idColumn, baseAlias, baseName string) (projected []string, idRequested bool, err error) {
	columns := q.Columns()
	if len(columns) > maxProjectionColumns {
		return nil, false, notSupported("more than 256 projection columns")
	}
	if len(columns) == 0 {
		if q.IDKind() != reflect.Invalid {
			params.Set("select", idColumn)
			return []string{idColumn}, true, nil
		}
		params.Set("select", "*")
		return nil, true, nil
	}

	parts := make([]string, 0, len(columns)+1)
	outputs := make([]string, 0, len(columns))
	seen := make(map[string]bool, len(columns))
	wildcard := false
	for _, column := range columns {
		if column.Wildcard != nil {
			if column.Wildcard.Source != "" || len(column.Wildcard.Exclude) != 0 {
				return nil, false, notSupported("wildcard projections with a source or exclusions")
			}
			if wildcard || len(parts) != 0 {
				return nil, false, notSupported("wildcards mixed with explicit columns")
			}
			wildcard = true
			parts = append(parts, "*")
			idRequested = true
			continue
		}
		if wildcard {
			// A wildcard followed by an explicit column used to be accepted and
			// then silently turned into select=*, dropping the projection. Refuse
			// both orders instead of losing a requested column.
			return nil, false, notSupported("wildcards mixed with explicit columns")
		}
		field, ok := column.Expression.(dal.FieldRef)
		if !ok {
			return nil, false, notSupported("projection expressions that are not plain fields")
		}
		if !validIdentifier(field.Name()) || !validFieldSource(field.Source(), baseAlias, baseName) {
			return nil, false, notSupported("field names must be simple SQL identifiers")
		}
		output := column.Alias
		if output == "" {
			output = field.Name()
		}
		if !validIdentifier(output) {
			return nil, false, notSupported("column aliases must be simple SQL identifiers")
		}
		if seen[output] {
			return nil, false, notSupported("duplicate output column names")
		}
		seen[output] = true
		if output == idColumn {
			if field.Name() != idColumn {
				return nil, false, notSupported("an alias that renames another field to the configured id column")
			}
			idRequested = true
		}
		if column.Alias != "" && column.Alias != field.Name() {
			parts = append(parts, column.Alias+":"+field.Name())
		} else {
			parts = append(parts, field.Name())
		}
		outputs = append(outputs, output)
	}
	if wildcard {
		params.Set("select", "*")
		return nil, true, nil
	}
	if !idRequested {
		parts = append(parts, idColumn)
	}
	if len(parts) == 0 {
		return nil, false, notSupported("an empty projection")
	}
	params.Set("select", strings.Join(parts, ","))
	return outputs, idRequested, nil
}

func addOrder(params url.Values, q dal.StructuredQuery, idColumn, baseAlias, baseName string) error {
	orders := q.OrderBy()
	parts := make([]string, 0, len(orders)+1)
	seen := make(map[string]bool, len(orders)+1)
	for _, order := range orders {
		if order == nil {
			return notSupported("nil order expressions")
		}
		field, ok := order.Expression().(dal.FieldRef)
		if !ok {
			return notSupported("ordering by expressions that are not plain fields")
		}
		if !validIdentifier(field.Name()) || !validFieldSource(field.Source(), baseAlias, baseName) {
			return notSupported("order fields must be simple SQL identifiers")
		}
		if seen[field.Name()] {
			return notSupported("duplicate order fields")
		}
		seen[field.Name()] = true
		direction := "asc"
		if order.Descending() {
			direction = "desc"
		}
		parts = append(parts, field.Name()+"."+direction)
	}
	if !seen[idColumn] {
		parts = append(parts, idColumn+".asc")
	}
	if len(parts) > maxOrderFields+1 {
		return notSupported("more than 16 ordering fields")
	}
	params.Set("order", strings.Join(parts, ","))
	return nil
}

type filterBuilder struct {
	params    url.Values
	baseAlias string
	baseName  string
	budget    int
}

func (f *filterBuilder) tick(depth int) error {
	if depth > maxFilterDepth {
		return notSupported("filters nested more than 8 levels deep")
	}
	f.budget++
	if f.budget > maxFilterConditions {
		return notSupported("more than 64 filter conditions")
	}
	return nil
}

// add handles the top-level AND chain: multiple parameter values are ANDed by
// PostgREST, while a top-level OR becomes one or=(...) parameter.
func (f *filterBuilder) add(condition dal.Condition, depth int) error {
	if condition == nil {
		return nil
	}
	if err := f.tick(depth); err != nil {
		return err
	}
	switch value := condition.(type) {
	case dal.GroupCondition:
		return f.addGroup(value, depth)
	case *dal.GroupCondition:
		if value == nil {
			return notSupported("a nil group condition")
		}
		return f.addGroup(*value, depth)
	case dal.Comparison:
		field, expression, err := f.comparison(value, false)
		if err != nil {
			return err
		}
		f.params.Add(field, expression)
		return nil
	case dal.IsNullCondition:
		field, expression, err := f.nullCondition(value)
		if err != nil {
			return err
		}
		f.params.Add(field, expression)
		return nil
	case *dal.IsNullCondition:
		if value == nil {
			return notSupported("a nil IS NULL condition")
		}
		field, expression, err := f.nullCondition(*value)
		if err != nil {
			return err
		}
		f.params.Add(field, expression)
		return nil
	default:
		return notSupported("this condition type")
	}
}

func (f *filterBuilder) addGroup(group dal.GroupCondition, depth int) error {
	conditions := group.Conditions()
	if len(conditions) == 0 {
		return notSupported("empty condition groups")
	}
	switch group.Operator() {
	case dal.And:
		for _, child := range conditions {
			if child == nil {
				return notSupported("a nil condition inside an AND group")
			}
			if err := f.add(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	case dal.Or:
		parts := make([]string, len(conditions))
		for i, child := range conditions {
			if child == nil {
				return notSupported("a nil condition inside an OR group")
			}
			expression, err := f.expression(child, depth+1)
			if err != nil {
				return err
			}
			parts[i] = expression
		}
		f.params.Add("or", "("+strings.Join(parts, ",")+")")
		return nil
	default:
		return notSupported("condition group operators other than AND and OR")
	}
}

func (f *filterBuilder) expression(condition dal.Condition, depth int) (string, error) {
	if condition == nil {
		return "", notSupported("a nil nested condition")
	}
	if err := f.tick(depth); err != nil {
		return "", err
	}
	switch value := condition.(type) {
	case dal.Comparison:
		field, expression, err := f.comparison(value, true)
		if err != nil {
			return "", err
		}
		return field + "." + expression, nil
	case dal.IsNullCondition:
		field, expression, err := f.nullCondition(value)
		if err != nil {
			return "", err
		}
		return field + "." + expression, nil
	case *dal.IsNullCondition:
		if value == nil {
			return "", notSupported("a nil IS NULL condition")
		}
		field, expression, err := f.nullCondition(*value)
		if err != nil {
			return "", err
		}
		return field + "." + expression, nil
	case dal.GroupCondition:
		return f.groupExpression(value, depth)
	case *dal.GroupCondition:
		if value == nil {
			return "", notSupported("a nil group condition")
		}
		return f.groupExpression(*value, depth)
	default:
		return "", notSupported("this nested condition type")
	}
}

func (f *filterBuilder) groupExpression(group dal.GroupCondition, depth int) (string, error) {
	conditions := group.Conditions()
	if len(conditions) == 0 {
		return "", notSupported("empty condition groups")
	}
	name := ""
	switch group.Operator() {
	case dal.And:
		name = "and"
	case dal.Or:
		name = "or"
	default:
		return "", notSupported("condition group operators other than AND and OR")
	}
	parts := make([]string, len(conditions))
	for i, child := range conditions {
		expression, err := f.expression(child, depth+1)
		if err != nil {
			return "", err
		}
		parts[i] = expression
	}
	return name + "(" + strings.Join(parts, ",") + ")", nil
}

func (f *filterBuilder) comparison(value dal.Comparison, quoteStrings bool) (field string, expression string, err error) {
	ref, ok := value.Left.(dal.FieldRef)
	if !ok || !validIdentifier(ref.Name()) || !validFieldSource(ref.Source(), f.baseAlias, f.baseName) {
		return "", "", notSupported("filter fields must be simple SQL identifiers of the read relation")
	}
	// A reserved PostgREST parameter name must be quoted; otherwise the whole
	// parameter is consumed as a control and the filter silently disappears.
	field = postgrestFieldKey(ref.Name())
	switch value.Operator {
	case dal.Equal:
		literal, kind, err := f.constantLiteral(value.Right, quoteStrings)
		if err != nil {
			return "", "", err
		}
		if kind == literalNull {
			// DALgo evaluates a comparison with NULL as UNKNOWN, never as a
			// match. PostgREST's is.null would match rows, so refuse instead of
			// changing the caller's meaning. Use dal.IsNullCondition explicitly.
			return "", "", notSupported("comparison to NULL is UNKNOWN; use dal.IsNullCondition or dal.IsNotNullCondition")
		}
		// An empty string is a value, not the NULL sentinel: its literal is also
		// "" but it renders as eq. (empty operand).
		return field, "eq." + literal, nil
	case dal.GreaterThen, dal.GreaterOrEqual, dal.LessThen, dal.LessOrEqual:
		literal, kind, err := f.constantLiteral(value.Right, quoteStrings)
		if err != nil {
			return "", "", err
		}
		if kind == literalNull {
			return "", "", notSupported("null values with this operator")
		}
		operators := map[dal.Operator]string{
			dal.GreaterThen:    "gt",
			dal.GreaterOrEqual: "gte",
			dal.LessThen:       "lt",
			dal.LessOrEqual:    "lte",
		}
		return field, operators[value.Operator] + "." + literal, nil
	case dal.In, dal.NotIn:
		values, err := membershipValues(value.Right)
		if err != nil {
			return "", "", err
		}
		parts := make([]string, len(values))
		for i, item := range values {
			if item == nil {
				return "", "", notSupported("null values in an IN or NOT IN list")
			}
			literal, err := scalarLiteral(item, true)
			if err != nil {
				return "", "", err
			}
			parts[i] = literal
		}
		operator := "in."
		if value.Operator == dal.NotIn {
			operator = "not.in."
		}
		return field, operator + "(" + strings.Join(parts, ",") + ")", nil
	default:
		return "", "", notSupported("filter operators other than equality, range, IN and NOT IN")
	}
}

// nullCondition renders DALgo's explicit IS NULL / IS NOT NULL predicate. Only a
// plain field of the read relation can be tested, because PostgREST's is.null
// operator applies to a column, not an arbitrary expression.
func (f *filterBuilder) nullCondition(value dal.IsNullCondition) (field, expression string, err error) {
	ref, ok := value.Operand().(dal.FieldRef)
	if !ok || !validIdentifier(ref.Name()) || !validFieldSource(ref.Source(), f.baseAlias, f.baseName) {
		return "", "", notSupported("IS NULL operands must be simple SQL identifiers of the read relation")
	}
	operator := "is.null"
	if value.Negated() {
		operator = "not.is.null"
	}
	return postgrestFieldKey(ref.Name()), operator, nil
}

// literalKind separates a rendered literal from its meaning. An empty string
// and the NULL sentinel both render "", so callers must not use the text alone
// to decide whether a constant is NULL.
type literalKind uint8

const (
	literalValue literalKind = iota
	literalNull
)

// constantLiteral renders a right-hand constant. Top-level scalar values are
// raw; membership lists and nested logical expressions quote strings.
func (f *filterBuilder) constantLiteral(expression dal.Expression, quoteStrings bool) (literal string, kind literalKind, err error) {
	var raw any
	switch value := expression.(type) {
	case dal.Constant:
		raw = value.Value
	case *dal.Constant:
		if value == nil {
			return "", literalValue, notSupported("a nil constant")
		}
		raw = value.Value
	default:
		return "", literalValue, notSupported("filter values that are not constants")
	}
	if raw == nil {
		return "", literalNull, nil
	}
	literal, err = scalarLiteral(raw, quoteStrings)
	if err != nil {
		return "", literalValue, err
	}
	return literal, literalValue, nil
}

func membershipValues(expression dal.Expression) ([]any, error) {
	var raw any
	switch value := expression.(type) {
	case dal.Array:
		raw = value.Value
	case *dal.Array:
		if value == nil {
			return nil, notSupported("a nil array")
		}
		raw = value.Value
	case dal.Constant:
		raw = value.Value
	case *dal.Constant:
		if value == nil {
			return nil, notSupported("a nil constant")
		}
		raw = value.Value
	default:
		return nil, notSupported("IN and NOT IN values that are not arrays")
	}
	if raw == nil {
		return nil, notSupported("a nil IN or NOT IN list")
	}
	value := reflect.ValueOf(raw)
	if value.Kind() != reflect.Slice && value.Kind() != reflect.Array {
		return nil, notSupported("IN and NOT IN values that are not arrays or slices")
	}
	length := value.Len()
	if length == 0 || length > maxMembershipValues {
		return nil, notSupported("IN and NOT IN lists must hold from 1 to 100 values")
	}
	values := make([]any, length)
	for i := 0; i < length; i++ {
		values[i] = value.Index(i).Interface()
	}
	return values, nil
}

// scalarLiteral renders one filter value. Top-level scalar filters (equality and
// range) are not a PostgREST list context, so a string there is sent raw and
// may contain commas, dots and quotes; wrapping it in double quotes would make
// them part of the compared value. Membership lists and nested logical
// expressions split on reserved punctuation, so quoteStrings is true there.
func scalarLiteral(value any, quoteStrings bool) (string, error) {
	switch value := value.(type) {
	case json.Number:
		if !reJSONNumber.MatchString(value.String()) {
			return "", notSupported("invalid JSON numeric literals")
		}
		return value.String(), nil
	case string:
		if len(value) > maxScalarBytes {
			return "", notSupported("filter string values longer than 4096 bytes")
		}
		if quoteStrings {
			return quotePostgrestString(value), nil
		}
		return value, nil
	case bool:
		if value {
			return "true", nil
		}
		return "false", nil
	case int:
		return strconv.FormatInt(int64(value), 10), nil
	case int8:
		return strconv.FormatInt(int64(value), 10), nil
	case int16:
		return strconv.FormatInt(int64(value), 10), nil
	case int32:
		return strconv.FormatInt(int64(value), 10), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case uint:
		return uintLiteral(uint64(value))
	case uint8:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint64:
		return uintLiteral(value)
	case float32:
		return floatLiteral(float64(value), 32)
	case float64:
		return floatLiteral(value, 64)
	case time.Time:
		text := value.Format(time.RFC3339Nano)
		if quoteStrings {
			return quotePostgrestString(text), nil
		}
		return text, nil
	default:
		return "", notSupported(fmt.Sprintf("filter values of type %T", value))
	}
}

func uintLiteral(value uint64) (string, error) {
	if value > math.MaxInt64 {
		return "", notSupported("unsigned integers above the PostgreSQL bigint range")
	}
	return strconv.FormatUint(value, 10), nil
}

func floatLiteral(value float64, bits int) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", notSupported("non-finite numbers")
	}
	return strconv.FormatFloat(value, 'g', -1, bits), nil
}

func quotePostgrestString(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
	return `"` + value + `"`
}
