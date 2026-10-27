package dalgo2postgrest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
	"github.com/dal-go/record"
)

const openAPIAccept = "application/openapi+json, application/json;q=0.9"

type openAPIDocument struct {
	Swagger     string                       `json:"swagger"`
	OpenAPI     string                       `json:"openapi"`
	Definitions map[string]openAPIDefinition `json:"definitions"`
	Components  *openAPIComponents           `json:"components"`
	Paths       map[string]json.RawMessage   `json:"paths"`
}

type openAPIComponents struct {
	Schemas map[string]openAPIDefinition `json:"schemas"`
}

type openAPIDefinition struct {
	Ref         string            `json:"$ref"`
	Type        string            `json:"type"`
	Format      string            `json:"format"`
	Description string            `json:"description"`
	Required    []string          `json:"required"`
	Properties  orderedProperties `json:"properties"`
}

type openAPIProperty struct {
	Ref         string           `json:"$ref"`
	Type        string           `json:"type"`
	Format      string           `json:"format"`
	Description string           `json:"description"`
	Items       *openAPIProperty `json:"items"`
	MaxLength   *int             `json:"maxLength"`
}

// orderedProperties keeps the property order PostgREST writes into OpenAPI.
// encoding/json's map form would otherwise lose the declared field order.
type orderedProperties struct {
	keys   []string
	values map[string]openAPIProperty
}

func (p *orderedProperties) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		p.keys = nil
		p.values = nil
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("postgrest: OpenAPI properties must be an object")
	}
	p.keys = nil
	p.values = make(map[string]openAPIProperty)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return errors.New("postgrest: OpenAPI property name must be a string")
		}
		var raw json.RawMessage
		if err = decoder.Decode(&raw); err != nil {
			return err
		}
		var property openAPIProperty
		if err = json.Unmarshal(raw, &property); err != nil {
			return err
		}
		p.keys = append(p.keys, key)
		p.values[key] = property
	}
	token, err = decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return errors.New("postgrest: OpenAPI properties object is not closed")
	}
	return nil
}

var _ dbschema.SchemaReader = (*Database)(nil)

// ListCollections returns the relations the active role can see in the
// PostgREST OpenAPI response. A valid document that exposes no relation to the
// role is an empty list. A missing, unauthorized or malformed document is an
// error, never an empty list.
func (d *Database) ListCollections(ctx context.Context, parent *record.Key) ([]dal.CollectionRef, error) {
	_ = parent
	if err := d.closedErr(); err != nil {
		return nil, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	opCtx, cancel := d.operationContext(ctx)
	defer cancel()
	document, err := d.fetchOpenAPIDocument(opCtx, "ListCollections")
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for name := range document.Definitions {
		if name != "" {
			names[name] = true
		}
	}
	if document.Components != nil {
		for name := range document.Components.Schemas {
			if name != "" {
				names[name] = true
			}
		}
	}
	for path := range document.Paths {
		if name, ok := relationFromPath(path); ok {
			names[name] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	collections := make([]dal.CollectionRef, len(ordered))
	for i, name := range ordered {
		collections[i] = dal.NewRootCollectionRef(name, "")
	}
	return collections, nil
}

// DescribeCollection maps one OpenAPI definition to a dbschema.CollectionDef.
// Field order follows the definition. Nullable follows OpenAPI's required list.
// A primary key is reported only when PostgREST's <pk/> marker is present;
// indexes, constraints and foreign keys are not invented.
func (d *Database) DescribeCollection(ctx context.Context, ref *dal.CollectionRef) (*dbschema.CollectionDef, error) {
	if err := d.closedErr(); err != nil {
		return nil, err
	}
	if ref == nil || ref.Name() == "" {
		return nil, fmt.Errorf("%w: a collection reference is required", ErrRelationNotFound)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if ref.Parent() != nil || ref.Database() != "" {
		return nil, notSupported("nested, collection-group or named-database metadata references")
	}
	if ref.Schema() != "" && ref.Schema() != d.cfg.schema {
		return nil, fmt.Errorf("%w: %q is not in the configured schema profile", ErrRelationNotFound, ref.Name())
	}
	opCtx, cancel := d.operationContext(ctx)
	defer cancel()
	document, err := d.fetchOpenAPIDocument(opCtx, "DescribeCollection")
	if err != nil {
		return nil, err
	}
	definition, ok := lookupDefinition(document, ref.Name())
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrRelationNotFound, ref.Name())
	}
	definition, err = resolveDefinition(document, definition, 0, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if len(definition.Properties.keys) == 0 {
		return nil, metadataUnavailable("DescribeCollection", "the OpenAPI definition has no properties")
	}
	required := make(map[string]bool, len(definition.Required))
	for _, name := range definition.Required {
		required[name] = true
	}
	fields := make([]dbschema.FieldDef, 0, len(definition.Properties.keys))
	sourceColumns := make([]dbschema.SourceColumnDef, 0, len(definition.Properties.keys))
	var primaryKey []dal.FieldName
	for _, name := range definition.Properties.keys {
		property, resolveErr := resolveProperty(document, definition.Properties.values[name], 0, map[string]bool{})
		if resolveErr != nil {
			return nil, resolveErr
		}
		nullable := !required[name]
		field := dbschema.FieldDef{
			Name:     dal.FieldName(name),
			Type:     schemaType(property),
			Nullable: nullable,
		}
		if property.MaxLength != nil && *property.MaxLength > 0 {
			length := *property.MaxLength
			field.Length = &length
		}
		fields = append(fields, field)
		declaredType := property.Format
		if declaredType == "" {
			declaredType = property.Type
		}
		source := dbschema.SourceColumnDef{
			Name:         name,
			DeclaredType: declaredType,
			NotNull:      !nullable,
		}
		if strings.Contains(property.Description, "<pk/>") {
			primaryKey = append(primaryKey, dal.FieldName(name))
			source.PrimaryKeyPosition = len(primaryKey)
		}
		sourceColumns = append(sourceColumns, source)
	}
	return &dbschema.CollectionDef{
		Name:       ref.Name(),
		Fields:     fields,
		PrimaryKey: primaryKey,
		SourceDefinition: &dbschema.SourceDefinition{
			Dialect: "postgrest-openapi",
			Columns: sourceColumns,
		},
	}, nil
}

func (d *Database) ListIndexes(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.IndexDef, error) {
	_ = ref
	if err := d.closedErr(); err != nil {
		return nil, err
	}
	return nil, &dbschema.NotSupportedError{
		Op:      "ListIndexes",
		Backend: "dalgo2postgrest",
		Reason:  "PostgREST OpenAPI exposes no index metadata",
	}
}

func (d *Database) ListConstraints(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.ConstraintDef, error) {
	_ = ref
	if err := d.closedErr(); err != nil {
		return nil, err
	}
	return nil, &dbschema.NotSupportedError{
		Op:      "ListConstraints",
		Backend: "dalgo2postgrest",
		Reason:  "PostgREST OpenAPI exposes no constraint metadata",
	}
}

func (d *Database) ListReferrers(ctx context.Context, ref *dal.CollectionRef) ([]dbschema.Referrer, error) {
	_ = ref
	if err := d.closedErr(); err != nil {
		return nil, err
	}
	return nil, &dbschema.NotSupportedError{
		Op:      "ListReferrers",
		Backend: "dalgo2postgrest",
		Reason:  "PostgREST OpenAPI exposes no foreign-key reference metadata",
	}
}

func (d *Database) fetchOpenAPIDocument(ctx context.Context, op string) (*openAPIDocument, error) {
	body, err := d.getBytes(ctx, "metadata", d.metadataURL(), openAPIAccept, d.cfg.maxMetadataBytes)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, ErrClosed) || errors.Is(err, ErrUnsafeHeader) {
			return nil, err
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			state := ErrMetadataUnavailable
			if httpErr.StatusCode == 401 || httpErr.StatusCode == 403 {
				state = ErrMetadataUnauthorized
			}
			return nil, &MetadataError{Op: op, StatusCode: httpErr.StatusCode, Err: state}
		}
		reason := "the root document could not be read"
		if errors.Is(err, ErrResponseTooLarge) {
			reason = "the root document exceeds the metadata byte budget"
		} else if errors.Is(err, ErrInvalidResponse) {
			reason = "the root document is not a complete JSON document"
		}
		return nil, &MetadataError{Op: op, Reason: reason, Err: ErrMetadataUnavailable}
	}
	var document openAPIDocument
	if err = json.Unmarshal(body, &document); err != nil {
		return nil, metadataUnavailable(op, "the root document is not valid OpenAPI JSON")
	}
	if document.Swagger == "" && document.OpenAPI == "" {
		return nil, metadataUnavailable(op, "the root document is not an OpenAPI document")
	}
	return &document, nil
}

func metadataUnavailable(op, reason string) error {
	return &MetadataError{Op: op, Reason: reason, Err: ErrMetadataUnavailable}
}

func lookupDefinition(document *openAPIDocument, name string) (openAPIDefinition, bool) {
	if document == nil {
		return openAPIDefinition{}, false
	}
	if definition, ok := document.Definitions[name]; ok {
		return definition, true
	}
	if document.Components != nil {
		if definition, ok := document.Components.Schemas[name]; ok {
			return definition, true
		}
	}
	return openAPIDefinition{}, false
}

func resolveDefinition(document *openAPIDocument, definition openAPIDefinition, depth int, seen map[string]bool) (openAPIDefinition, error) {
	if definition.Ref == "" {
		return definition, nil
	}
	if depth >= 8 {
		return openAPIDefinition{}, metadataUnavailable("DescribeCollection", "an OpenAPI definition ref is nested too deeply")
	}
	name, ok := localRefName(definition.Ref)
	if !ok {
		return openAPIDefinition{}, metadataUnavailable("DescribeCollection", "an OpenAPI definition uses a non-local ref; cross-origin refs are never fetched")
	}
	if seen[definition.Ref] {
		return openAPIDefinition{}, metadataUnavailable("DescribeCollection", "an OpenAPI definition ref is cyclic")
	}
	target, ok := lookupDefinition(document, name)
	if !ok {
		return openAPIDefinition{}, metadataUnavailable("DescribeCollection", "an OpenAPI definition ref cannot be resolved")
	}
	seen[definition.Ref] = true
	return resolveDefinition(document, target, depth+1, seen)
}

func resolveProperty(document *openAPIDocument, property openAPIProperty, depth int, seen map[string]bool) (openAPIProperty, error) {
	if property.Ref == "" {
		return property, nil
	}
	if depth >= 8 {
		return openAPIProperty{}, metadataUnavailable("DescribeCollection", "an OpenAPI property ref is nested too deeply")
	}
	name, ok := localRefName(property.Ref)
	if !ok {
		return openAPIProperty{}, metadataUnavailable("DescribeCollection", "an OpenAPI property uses a non-local ref; cross-origin refs are never fetched")
	}
	if seen[property.Ref] {
		return openAPIProperty{}, metadataUnavailable("DescribeCollection", "an OpenAPI property ref is cyclic")
	}
	target, ok := lookupDefinition(document, name)
	if !ok {
		return openAPIProperty{}, metadataUnavailable("DescribeCollection", "an OpenAPI property ref cannot be resolved")
	}
	seen[property.Ref] = true
	resolved := openAPIProperty{
		Ref:         target.Ref,
		Type:        target.Type,
		Format:      target.Format,
		Description: target.Description,
	}
	if resolved.Type == "" && len(target.Properties.keys) != 0 {
		resolved.Type = "object"
	}
	return resolveProperty(document, resolved, depth+1, seen)
}

func localRefName(ref string) (string, bool) {
	for _, prefix := range []string{"#/definitions/", "#/components/schemas/"} {
		if strings.HasPrefix(ref, prefix) {
			name := strings.TrimPrefix(ref, prefix)
			name = strings.ReplaceAll(name, "~1", "/")
			name = strings.ReplaceAll(name, "~0", "~")
			if name == "" {
				return "", false
			}
			return name, true
		}
	}
	return "", false
}

func relationFromPath(path string) (string, bool) {
	if path == "/" || !strings.HasPrefix(path, "/") {
		return "", false
	}
	segment := strings.TrimPrefix(path, "/")
	if segment == "" || strings.Contains(segment, "/") || strings.HasPrefix(segment, "rpc/") {
		return "", false
	}
	name, err := url.PathUnescape(segment)
	if err != nil || name == "" {
		return "", false
	}
	return name, true
}

func schemaType(property openAPIProperty) dbschema.Type {
	switch strings.ToLower(property.Type) {
	case "integer":
		return dbschema.Int
	case "number":
		format := strings.ToLower(property.Format)
		if strings.Contains(format, "float") || strings.Contains(format, "double") || format == "real" {
			return dbschema.Float
		}
		return dbschema.Decimal
	case "boolean":
		return dbschema.Bool
	case "string":
		switch strings.ToLower(property.Format) {
		case "byte", "binary", "bytea":
			return dbschema.Bytes
		case "date", "time", "date-time", "timestamp", "timestamptz",
			"timestamp with time zone", "timestamp without time zone":
			return dbschema.Time
		}
		return dbschema.String
	default:
		// Arrays, objects, json/jsonb and formats dbschema.Type cannot express
		// stay Null; SourceDefinition.DeclaredType keeps the native format.
		return dbschema.Null
	}
}
