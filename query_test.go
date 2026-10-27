package dalgo2postgrest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dalgotest"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

func consumeRecords(t *testing.T, reader dal.RecordsReader) []record.Record {
	t.Helper()
	var records []record.Record
	for {
		rec, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			return records
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		records = append(records, rec)
	}
}

func TestQueryTranslation(t *testing.T) {
	var captured url.Values
	var headers http.Header
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/items" {
			t.Errorf("path = %q", r.URL.Path)
		}
		captured = r.URL.Query()
		headers = r.Header.Clone()
		writeJSON(w, "[]")
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("qty", dal.GreaterOrEqual, 10).
		WhereField("title", dal.Equal, `a"b\c`).
		WhereField("id", dal.In, []int64{1, 2}).
		OrderBy(dal.DescendingField("qty")).
		Limit(3).
		Offset(1).
		SelectColumns(
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Alias: "q", Expression: dal.Field("qty")},
		)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if records := consumeRecords(t, reader); len(records) != 0 {
		t.Fatalf("records = %d", len(records))
	}
	if got := server.hits.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if got := captured.Get("select"); got != "title,q:qty,id" {
		t.Fatalf("select = %q", got)
	}
	if got := captured.Get("order"); got != "qty.desc,id.asc" {
		t.Fatalf("order = %q", got)
	}
	if got := captured.Get("limit"); got != "3" {
		t.Fatalf("limit = %q", got)
	}
	if got := captured.Get("offset"); got != "1" {
		t.Fatalf("offset = %q", got)
	}
	if got := captured.Get("qty"); got != "gte.10" {
		t.Fatalf("qty = %q", got)
	}
	if got := captured.Get("title"); got != `eq.a"b\c` {
		t.Fatalf("title = %q", got)
	}
	if got := captured.Get("id"); got != "in.(1,2)" {
		t.Fatalf("id = %q", got)
	}
	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := headers.Get("Authorization"); got != "Bearer unit-test" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := headers.Get("Prefer"); got != "" {
		t.Fatalf("Prefer = %q, want empty", got)
	}
	if _, exists := captured["count"]; exists {
		t.Fatal("a count parameter was sent")
	}
}

func TestQueryProjectionAndStableOrder(t *testing.T) {
	var query url.Values
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(w, `[{"id":2,"title":"two"}]`)
	})
	q := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Limit(1).
		SelectColumns(dal.Column{Expression: dal.Field("title")})
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if got := query.Get("select"); got != "title,id" {
		t.Fatalf("select = %q", got)
	}
	if got := query.Get("order"); got != "id.asc" {
		t.Fatalf("order = %q", got)
	}
	if got := records[0].Key().ID; got != int64(2) {
		t.Fatalf("key id = %#v (%T)", got, got)
	}
	data := records[0].Data().(map[string]any)
	if _, found := data["id"]; found {
		t.Fatalf("projection leaked the hidden id column: %#v", data)
	}
	if data["title"] != "two" {
		t.Fatalf("data = %#v", data)
	}
}

func TestQueryKeepsRequestedIDAndWildcard(t *testing.T) {
	for _, test := range []struct {
		name        string
		query       dal.Query
		selectValue string
	}{
		{"explicit id", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("id")}, dal.Column{Expression: dal.Field("title")}), "id,title"},
		{"wildcard", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil), "*"},
		{"keys only", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectKeysOnly(reflect.Int64), "id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var query url.Values
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				writeJSON(w, `[{"id":2,"title":"two"}]`)
			})
			reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			records := consumeRecords(t, reader)
			if got := query.Get("select"); got != test.selectValue {
				t.Fatalf("select = %q, want %q", got, test.selectValue)
			}
			data := records[0].Data().(map[string]any)
			if data["id"] != int64(2) {
				t.Fatalf("id in data = %#v", data)
			}
		})
	}
}

func TestFilterRendering(t *testing.T) {
	var query url.Values
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(w, "[]")
	})
	group := dal.NewGroupCondition(dal.Or,
		dal.WhereField("qty", dal.Equal, 10),
		dal.NewGroupCondition(dal.And,
			dal.WhereField("qty", dal.GreaterOrEqual, 20),
			dal.WhereField("active", dal.Equal, true),
			dal.NewIsNotNullCondition(dal.Field("archived_at")),
		),
	)
	q := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(
			dal.NewGroupCondition(dal.And,
				dal.NewIsNullCondition(dal.Field("deleted_at")),
				dal.WhereField("id", dal.NotIn, []int{1, 2}),
				dal.WhereField("created_at", dal.GreaterOrEqual, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)),
			),
		).
		Where(group).
		Limit(1).
		SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	_ = consumeRecords(t, reader)
	if got := query.Get("deleted_at"); got != "is.null" {
		t.Fatalf("deleted_at = %q", got)
	}
	if got := query.Get("id"); got != "not.in.(1,2)" {
		t.Fatalf("id = %q", got)
	}
	if got := query.Get("created_at"); got != "gte.2024-01-01T00:00:00Z" {
		t.Fatalf("created_at = %q", got)
	}
	if got := query.Get("or"); got != "(qty.eq.10,and(qty.gte.20,active.eq.true,archived_at.not.is.null))" {
		t.Fatalf("or = %q", got)
	}
}

func TestReservedFilterNamesAreQuoted(t *testing.T) {
	for _, name := range []string{"select", "columns", "on_conflict", "order", "limit", "offset", "and", "or"} {
		t.Run(name, func(t *testing.T) {
			var query url.Values
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				writeJSON(w, "[]")
			})
			q := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
				WhereField(name, dal.Equal, 5).
				Limit(1).
				SelectIntoRecord(nil)
			reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			_ = consumeRecords(t, reader)

			quoted := `"` + name + `"`
			if got := query.Get(quoted); got != "eq.5" {
				t.Fatalf("filter on %q = %q, want eq.5 (quoted key %q missing)", name, got, quoted)
			}
			switch name {
			case "select":
				if got := query.Get("select"); got != "*" {
					t.Fatalf("select control = %q, want *", got)
				}
			case "limit":
				if got := query.Get("limit"); got != "1" {
					t.Fatalf("limit control = %q, want 1", got)
				}
			case "offset":
				if got := query.Get("offset"); got != "0" {
					t.Fatalf("offset control = %q, want 0", got)
				}
			case "order":
				if got := query.Get("order"); got != "id.asc" {
					t.Fatalf("order control = %q, want id.asc", got)
				}
			default:
				if _, exists := query[name]; exists {
					t.Fatalf("unquoted %q parameter was sent: %v", name, query[name])
				}
			}
			if got := server.hits.Load(); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestReservedFilterNamesInNestedAndExplicitPredicates(t *testing.T) {
	var query url.Values
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(w, "[]")
	})
	q := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(dal.NewGroupCondition(dal.Or,
			dal.WhereField("limit", dal.GreaterThen, 1),
			dal.WhereField("offset", dal.In, []int{1, 2}),
			dal.WhereField("columns", dal.NotIn, []string{"x"}),
			dal.NewIsNullCondition(dal.Field("select")),
		)).
		Where(dal.Field("or").IsNull()).
		Where(dal.Field("and").IsNotNull()).
		WhereField("select", dal.NotIn, []string{"x"}).
		WhereField("order", dal.LessOrEqual, 3).
		Limit(1).
		SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	_ = consumeRecords(t, reader)

	if got, want := query.Get("or"), `("limit".gt.1,"offset".in.(1,2),"columns".not.in.("x"),"select".is.null)`; got != want {
		t.Fatalf("or = %q, want %q", got, want)
	}
	if got := query.Get(`"or"`); got != "is.null" {
		t.Fatalf("IS NULL on or = %q, want is.null", got)
	}
	if got := query.Get(`"and"`); got != "not.is.null" {
		t.Fatalf("IS NOT NULL on and = %q, want not.is.null", got)
	}
	if got := query.Get(`"select"`); got != `not.in.("x")` {
		t.Fatalf("NOT IN on select = %q", got)
	}
	if got := query.Get(`"order"`); got != "lte.3" {
		t.Fatalf("range on order = %q, want lte.3", got)
	}
	if got := query.Get("limit"); got != "1" {
		t.Fatalf("limit control = %q, want 1", got)
	}
	if got := query.Get("offset"); got != "0" {
		t.Fatalf("offset control = %q, want 0", got)
	}
}

func TestOrdinaryFilterNamesStayUnquoted(t *testing.T) {
	var query url.Values
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(w, "[]")
	})
	q := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("limit_count", dal.Equal, 5).
		WhereField("oracle", dal.Equal, "x").
		WhereField("ordered", dal.GreaterThen, 1).
		Limit(1).
		SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	_ = consumeRecords(t, reader)
	for name, want := range map[string]string{
		"limit_count": "eq.5",
		"oracle":      "eq.x",
		"ordered":     "gt.1",
	} {
		if got := query.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
		if _, exists := query[`"`+name+`"`]; exists {
			t.Fatalf("ordinary field %q was quoted", name)
		}
	}
}

func TestNullSemantics(t *testing.T) {
	var query url.Values
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		writeJSON(w, "[]")
	})
	ctx := context.Background()

	// A zero-length string is a value. It must not be rewritten as is.null.
	empty := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("title", dal.Equal, "").
		Limit(1).
		SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(ctx, empty)
	if err != nil {
		t.Fatalf("empty string query: %v", err)
	}
	_ = consumeRecords(t, reader)
	if got := query.Get("title"); got != "eq." {
		t.Fatalf("empty string filter = %q, want eq.", got)
	}
	if got := server.hits.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}

	// DALgo defines a comparison with NULL as UNKNOWN (q_null_test.go), so the
	// adapter must refuse it before I/O rather than match NULL rows with is.null.
	nullComparison := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("deleted_at", dal.Equal, nil).
		Limit(1).
		SelectIntoRecord(nil)
	if _, err = server.db.ExecuteQueryToRecordsReader(ctx, nullComparison); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("comparison to NULL = %v, want ErrNotSupported", err)
	}
	nestedNull := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(dal.NewGroupCondition(dal.Or, dal.WhereField("deleted_at", dal.Equal, nil))).
		Limit(1).
		SelectIntoRecord(nil)
	if _, err = server.db.ExecuteQueryToRecordsReader(ctx, nestedNull); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("nested comparison to NULL = %v, want ErrNotSupported", err)
	}
	if got := server.hits.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1; a NULL comparison reached the network", got)
	}

	// Explicit IS NULL / IS NOT NULL is the supported way to test NULL.
	isNull := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(dal.Field("deleted_at").IsNull()).
		Limit(1).
		SelectIntoRecord(nil)
	reader, err = server.db.ExecuteQueryToRecordsReader(ctx, isNull)
	if err != nil {
		t.Fatalf("IS NULL query: %v", err)
	}
	_ = consumeRecords(t, reader)
	if got := query.Get("deleted_at"); got != "is.null" {
		t.Fatalf("IS NULL filter = %q", got)
	}
	notNull := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(dal.Field("deleted_at").IsNotNull()).
		Limit(1).
		SelectIntoRecord(nil)
	reader, err = server.db.ExecuteQueryToRecordsReader(ctx, notNull)
	if err != nil {
		t.Fatalf("IS NOT NULL query: %v", err)
	}
	_ = consumeRecords(t, reader)
	if got := query.Get("deleted_at"); got != "not.is.null" {
		t.Fatalf("IS NOT NULL filter = %q", got)
	}
}

func TestProjectionWildcardMixingAndAliases(t *testing.T) {
	wildcard := dal.AllColumnsExcept()
	for _, test := range []struct {
		name  string
		query dal.Query
	}{
		{"wildcard first", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(wildcard, dal.Column{Expression: dal.Field("title")})},
		{"wildcard last", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")}, wildcard)},
		{"wildcard after alias", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Alias: "q", Expression: dal.Field("qty")}, wildcard)},
		{"wildcard before alias", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(wildcard, dal.Column{Alias: "q", Expression: dal.Field("qty")})},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("a mixed wildcard projection reached the network")
				writeJSON(w, "[]")
			})
			if _, err := server.db.ExecuteQueryToRecordsReader(context.Background(), test.query); !errors.Is(err, dal.ErrNotSupported) {
				t.Fatalf("query = %v, want ErrNotSupported", err)
			}
			if got := server.hits.Load(); got != 0 {
				t.Fatalf("requests = %d, want 0", got)
			}
		})
	}

	for _, test := range []struct {
		name        string
		query       dal.Query
		selectValue string
	}{
		{"aliases", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(
				dal.Column{Alias: "q", Expression: dal.Field("qty")},
				dal.Column{Expression: dal.Field("title")},
			), "q:qty,title,id"},
		{"id only", dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("id")}), "id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var query url.Values
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				// PostgREST returns the output name of an aliased projection
				// ("q:qty" arrives as "q"), which the adapter now enforces.
				writeJSON(w, `[{"id":1,"title":"one","q":10}]`)
			})
			reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			_ = consumeRecords(t, reader)
			if got := query.Get("select"); got != test.selectValue {
				t.Fatalf("select = %q, want %q", got, test.selectValue)
			}
		})
	}
}

func TestQueryUnsupportedBeforeIO(t *testing.T) {
	textQuery := dal.NewTextQuery("select * from items", nil)
	joinedFrom := dal.From(dal.NewRootCollectionRef("items", ""))
	joinedFrom.Join(dal.NewJoinedSource(dal.NewRootCollectionRef("other", ""), dal.JoinInner,
		dal.Comparison{Left: dal.NewFieldRef("items", "id"), Operator: dal.Equal, Right: dal.NewFieldRef("other", "item_id")}))
	joinQuery := joinedFrom.NewQuery().Limit(1).SelectIntoRecord(nil)
	inner := dal.From(dal.NewRootCollectionRef("inner", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	nestedQuery := dal.From(dal.NewQuerySource(inner, "inner")).NewQuery().Limit(1).SelectIntoRecord(nil)
	aggregateQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		GroupBy(dal.Field("qty")).
		Limit(5).
		SelectColumns(dal.Count())
	deep := dal.NewGroupCondition(dal.Or, dal.WhereField("a", dal.Equal, 1))
	for i := 0; i < 10; i++ {
		deep = dal.NewGroupCondition(dal.And, deep, dal.WhereField(fmt.Sprintf("f%d", i), dal.Equal, i))
	}
	manyValues := make([]int, 101)
	for i := range manyValues {
		manyValues[i] = i
	}
	manyColumns := make([]dal.Column, 257)
	for i := range manyColumns {
		manyColumns[i] = dal.Column{Expression: dal.Field(fmt.Sprintf("f%d", i))}
	}
	longFilters := make([]dal.Condition, 50)
	for i := range longFilters {
		longFilters[i] = dal.WhereField(fmt.Sprintf("f%d", i), dal.Equal, strings.Repeat("x", 1000))
	}
	cases := []struct {
		name   string
		config func(*Config)
		query  dal.Query
	}{
		{"native SQL", nil, textQuery},
		{"join", nil, joinQuery},
		{"nested source", nil, nestedQuery},
		{"aggregate", nil, aggregateQuery},
		{"cursor", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).StartAfter("x").SelectIntoRecord(nil)},
		{"field-to-field filter", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			Where(dal.Comparison{Left: dal.Field("a"), Operator: dal.Equal, Right: dal.Field("b")}).SelectIntoRecord(nil)},
		{"empty group", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			Where(dal.NewGroupCondition(dal.And)).SelectIntoRecord(nil)},
		{"null range", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			WhereField("id", dal.GreaterThen, nil).SelectIntoRecord(nil)},
		{"null equality", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			WhereField("id", dal.Equal, nil).SelectIntoRecord(nil)},
		{"is null without a field", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			Where(dal.NewIsNullCondition(nil)).SelectIntoRecord(nil)},
		{"unknown operator", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			Where(dal.Comparison{Left: dal.Field("id"), Operator: "LIKE", Right: dal.Constant{Value: "x"}}).SelectIntoRecord(nil)},
		{"empty membership", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			WhereField("id", dal.In, []int{}).SelectIntoRecord(nil)},
		{"too many projection columns", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(manyColumns...)},
		{"oversized filter value", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			WhereField("title", dal.Equal, strings.Repeat("x", maxScalarBytes+1)).SelectIntoRecord(nil)},
		{"oversized query parameters", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			Where(longFilters...).SelectIntoRecord(nil)},
		{"oversized membership", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			WhereField("id", dal.In, manyValues).SelectIntoRecord(nil)},
		{"deep filters", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).Where(deep).SelectIntoRecord(nil)},
		{"wildcard exclusion", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.AllColumnsExcept("secret"))},
		{"alias renames id", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Alias: "id", Expression: dal.Field("title")})},
		{"duplicate output", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")}, dal.Column{Alias: "title", Expression: dal.Field("qty")})},
		{"duplicate order", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			OrderBy(dal.AscendingField("qty"), dal.DescendingField("qty")).SelectIntoRecord(nil)},
		{"unsafe relation", nil, dal.From(dal.NewRootCollectionRef("bad-name", "")).NewQuery().Limit(1).SelectIntoRecord(nil)},
		{"schema mismatch", func(c *Config) { c.Schema = "app" },
			dal.From(dal.NewQualifiedRootCollectionRef("other", "items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)},
		{"unbounded", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().SelectIntoRecord(nil)},
		{"negative limit", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(-1).SelectIntoRecord(nil)},
		{"over row budget", func(c *Config) { c.MaxRows = 2 },
			dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(3).SelectIntoRecord(nil)},
		{"negative offset", nil, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).Offset(-1).SelectIntoRecord(nil)},
		{"over offset budget", func(c *Config) { c.MaxOffset = 2 },
			dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).Offset(3).SelectIntoRecord(nil)},
		{"over page budget", func(c *Config) { c.MaxPageSize = 1; c.MaxRequests = 2 },
			dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(3).SelectIntoRecord(nil)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mutate := []func(*Config){}
			if test.config != nil {
				mutate = append(mutate, test.config)
			}
			server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Error("an HTTP request was sent for an unsupported query")
				writeJSON(w, "[]")
			}, mutate...)
			_, err := server.db.ExecuteQueryToRecordsReader(context.Background(), test.query)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, dal.ErrNotSupported) && !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("error = %v, want ErrNotSupported or ErrBudgetExceeded", err)
			}
			if got := server.hits.Load(); got != 0 {
				t.Fatalf("requests = %d, want 0", got)
			}
		})
	}
}

func TestGenericAggregateFallbackRefusesUnboundedSource(t *testing.T) {
	aggregate := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		GroupBy(dal.Field("qty")).
		Limit(5).
		SelectColumns(dal.Count())
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("generic aggregation reached the network")
		writeJSON(w, "[]")
	})
	if _, err := server.db.ExecuteQueryToRecordsReader(context.Background(), aggregate); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("records reader aggregate = %v, want ErrNotSupported", err)
	}
	if _, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), aggregate); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("recordset reader aggregate = %v, want ErrNotSupported", err)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestQueryPagination(t *testing.T) {
	dataset := []string{
		`{"id":5,"title":"five"}`,
		`{"id":4,"title":"four"}`,
		`{"id":3,"title":"three"}`,
		`{"id":2,"title":"two"}`,
		`{"id":1,"title":"one"}`,
	}
	var offsets []string
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		offset := 0
		fmt.Sscanf(query.Get("offset"), "%d", &offset)
		limit := 0
		fmt.Sscanf(query.Get("limit"), "%d", &limit)
		offsets = append(offsets, query.Get("offset")+"/"+query.Get("limit"))
		end := offset + limit
		if end > len(dataset) {
			end = len(dataset)
		}
		if offset > len(dataset) {
			offset = len(dataset)
		}
		writeJSON(w, "["+strings.Join(dataset[offset:end], ",")+"]")
	}, func(c *Config) {
		c.MaxPageSize = 2
		c.MaxRequests = 5
		c.MaxRows = 10
		c.MaxOffset = 10
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	dalgotest.AssertUniqueKeys(t, records)
	if len(records) != 5 {
		t.Fatalf("records = %d, want 5", len(records))
	}
	for i, want := range []int64{5, 4, 3, 2, 1} {
		if got := records[i].Key().ID; got != want {
			t.Fatalf("record %d id = %#v, want %d", i, got, want)
		}
	}
	if got := strings.Join(offsets, ","); got != "0/2,2/2,4/1" {
		t.Fatalf("page offsets = %s", got)
	}
	if got := server.hits.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestShortFinalPageIsNotDiscarded(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") != "0" {
			writeJSON(w, "[]")
			return
		}
		writeJSON(w, `[{"id":3},{"id":2},{"id":1}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3; a short final page was discarded", len(records))
	}
	if got := server.hits.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (the short page plus a verified empty page)", got)
	}
}

func TestRecordsetReaderPaginationAndColumns(t *testing.T) {
	dataset := []string{
		`{"id":2,"title":"two","qty":20}`,
		`{"id":1,"title":"one","qty":10}`,
	}
	var hits int
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		query := r.URL.Query()
		offset := 0
		fmt.Sscanf(query.Get("offset"), "%d", &offset)
		limit := 0
		fmt.Sscanf(query.Get("limit"), "%d", &limit)
		end := offset + limit
		if end > len(dataset) {
			end = len(dataset)
		}
		writeJSON(w, "["+strings.Join(dataset[offset:end], ",")+"]")
	}, func(c *Config) {
		c.MaxPageSize = 1
		c.MaxRequests = 3
		c.MaxRows = 10
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Limit(2).
		OrderBy(dal.DescendingField("qty")).
		SelectIntoRecordset()
	reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query, recordset.WithName("custom"))
	if err != nil {
		t.Fatal(err)
	}
	rs := reader.Recordset()
	if rs.Name() != "custom" {
		t.Fatalf("name = %q", rs.Name())
	}
	columns := rs.Columns()
	if len(columns) != 3 || columns[0].Name() != "id" || columns[1].Name() != "qty" || columns[2].Name() != "title" {
		t.Fatalf("columns = %#v", columns)
	}
	var rows int
	for {
		row, gotRS, err := reader.Next()
		if errors.Is(err, dal.ErrNoMoreRecords) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		value, err := row.GetValueByName("title", gotRS)
		if err != nil {
			t.Fatal(err)
		}
		if value == "" {
			t.Fatal("empty title")
		}
		rows++
	}
	if rows != 2 {
		t.Fatalf("rows = %d", rows)
	}
	if hits != 2 {
		t.Fatalf("requests = %d, want 2", hits)
	}
}

func TestRecordsetExplicitProjectionKeepsOrderAndEmptyShape(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("select"); got != "title,q:qty,id" {
			t.Errorf("select = %q", got)
		}
		writeJSON(w, "[]")
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
		SelectColumns(
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Alias: "q", Expression: dal.Field("qty")},
		)
	reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	rs := reader.Recordset()
	columns := rs.Columns()
	if len(columns) != 2 || columns[0].Name() != "title" || columns[1].Name() != "q" {
		t.Fatalf("columns = %#v", columns)
	}
	if _, _, err = reader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
		t.Fatalf("Next = %v, want ErrNoMoreRecords", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reader.Next(); !errors.Is(err, dal.ErrReaderClosed) {
		t.Fatalf("Next after Close = %v, want ErrReaderClosed", err)
	}
}
