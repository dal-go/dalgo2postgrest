package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

const openAPIFixture = `{
  "swagger": "2.0",
  "definitions": {
    "items": {
      "required": ["id", "title"],
      "properties": {
        "id": {"description": "Note:\nThis is a Primary Key.<pk/>", "format": "int64", "type": "integer"},
        "title": {"format": "text", "type": "string"},
        "price": {"format": "numeric", "type": "number"},
        "active": {"format": "boolean", "type": "boolean"},
        "created_at": {"format": "timestamp with time zone", "type": "string"},
        "tags": {"format": "text[]", "type": "array", "items": {"type": "string"}},
        "doc": {"format": "jsonb"}
      },
      "type": "object"
    }
  },
  "paths": {"/": {}, "/items": {}, "/rpc/do": {}}
}`

func TestMetadataListAndDescribe(t *testing.T) {
	var headers http.Header
	var path string
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		headers = r.Header.Clone()
		writeJSON(w, openAPIFixture)
	}, func(c *Config) { c.Schema = "app" })

	collections, err := server.db.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(collections) != 1 || collections[0].Name() != "items" {
		t.Fatalf("collections = %#v", collections)
	}
	if path != "/rest/v1" {
		t.Fatalf("metadata path = %q", path)
	}
	if got := headers.Get("Accept-Profile"); got != "app" {
		t.Fatalf("Accept-Profile = %q", got)
	}
	if got := headers.Get("Accept"); !strings.Contains(got, "application/openapi+json") {
		t.Fatalf("Accept = %q", got)
	}

	ref := collections[0]
	definition, err := server.db.DescribeCollection(context.Background(), &ref)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if definition.Name != "items" {
		t.Fatalf("name = %q", definition.Name)
	}
	wantTypes := []dbschema.Type{
		dbschema.Int,
		dbschema.String,
		dbschema.Decimal,
		dbschema.Bool,
		dbschema.Time,
		dbschema.Null,
		dbschema.Null,
	}
	if len(definition.Fields) != len(wantTypes) {
		t.Fatalf("fields = %#v", definition.Fields)
	}
	for i, want := range wantTypes {
		if definition.Fields[i].Type != want {
			t.Fatalf("field %d (%s) type = %v, want %v", i, definition.Fields[i].Name, definition.Fields[i].Type, want)
		}
	}
	if definition.Fields[0].Name != "id" || definition.Fields[1].Name != "title" || definition.Fields[2].Name != "price" {
		t.Fatalf("field order = %#v", definition.Fields)
	}
	if definition.Fields[0].Nullable || definition.Fields[1].Nullable {
		t.Fatal("required fields were reported nullable")
	}
	if !definition.Fields[2].Nullable {
		t.Fatal("an optional field was reported not-null")
	}
	if len(definition.PrimaryKey) != 1 || definition.PrimaryKey[0] != "id" {
		t.Fatalf("primary key = %#v", definition.PrimaryKey)
	}
	if definition.SourceDefinition == nil || definition.SourceDefinition.Dialect != "postgrest-openapi" {
		t.Fatalf("source definition = %#v", definition.SourceDefinition)
	}
	if got := definition.SourceDefinition.Columns[0].DeclaredType; got != "int64" {
		t.Fatalf("declared type = %q", got)
	}
	if got := definition.SourceDefinition.Columns[6].DeclaredType; got != "jsonb" {
		t.Fatalf("declared type for doc = %q", got)
	}

	hits := server.hits.Load()
	if _, err = server.db.ListIndexes(context.Background(), &ref); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("ListIndexes = %v, want ErrNotSupported", err)
	}
	if _, err = server.db.ListConstraints(context.Background(), &ref); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("ListConstraints = %v, want ErrNotSupported", err)
	}
	if _, err = server.db.ListReferrers(context.Background(), &ref); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("ListReferrers = %v, want ErrNotSupported", err)
	}
	if server.hits.Load() != hits {
		t.Fatal("unsupported metadata operations performed HTTP I/O")
	}
}

func TestMetadataUnauthorizedAndUnavailable(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		const secret = "metadata-secret"
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"` + secret + `"}`))
		})
		_, err := server.db.ListCollections(context.Background(), nil)
		if !errors.Is(err, ErrMetadataUnauthorized) {
			t.Fatalf("error = %v, want ErrMetadataUnauthorized", err)
		}
		var metadataErr *MetadataError
		if !errors.As(err, &metadataErr) || metadataErr.StatusCode != 403 {
			t.Fatalf("metadata error = %#v", metadataErr)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked the body: %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrMetadataUnavailable) {
			t.Fatalf("error = %v, want ErrMetadataUnavailable", err)
		}
	})

	t.Run("not OpenAPI", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{"tables":["items"]}`)
		})
		if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrMetadataUnavailable) {
			t.Fatalf("error = %v, want ErrMetadataUnavailable", err)
		}
	})

	t.Run("malformed OpenAPI", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, `{`)
		})
		if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrMetadataUnavailable) {
			t.Fatalf("error = %v, want ErrMetadataUnavailable", err)
		}
	})

	t.Run("too large", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, openAPIFixture+"                         ")
		}, func(c *Config) { c.MaxMetadataBytes = 20 })
		if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrMetadataUnavailable) {
			t.Fatalf("error = %v, want ErrMetadataUnavailable", err)
		}
	})
}

func TestMetadataEmptyDocumentIsNotAnError(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `{"swagger":"2.0","paths":{"/":{}}}`)
	})
	collections, err := server.db.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(collections) != 0 {
		t.Fatalf("collections = %#v, want none", collections)
	}
}

func TestMetadataUnknownRelation(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAPIFixture)
	})
	ref := dal.NewRootCollectionRef("missing", "")
	if _, err := server.db.DescribeCollection(context.Background(), &ref); !errors.Is(err, ErrRelationNotFound) {
		t.Fatalf("error = %v, want ErrRelationNotFound", err)
	}
}

func TestMetadataLocalAndExternalRefs(t *testing.T) {
	t.Run("local ref", func(t *testing.T) {
		body := `{
			"swagger":"2.0",
			"definitions":{
				"status":{"type":"string","format":"text"},
				"items":{"type":"object","properties":{"status":{"$ref":"#/definitions/status"}}}
			},
			"paths":{"/items":{}}
		}`
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
		ref := dal.NewRootCollectionRef("items", "")
		definition, err := server.db.DescribeCollection(context.Background(), &ref)
		if err != nil {
			t.Fatal(err)
		}
		if len(definition.Fields) != 1 || definition.Fields[0].Type != dbschema.String {
			t.Fatalf("fields = %#v", definition.Fields)
		}
	})

	t.Run("external ref is never fetched", func(t *testing.T) {
		body := `{
			"swagger":"2.0",
			"definitions":{"items":{"type":"object","properties":{"weird":{"$ref":"https://evil.example/schema.json"}}}},
			"paths":{"/items":{}}
		}`
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
		ref := dal.NewRootCollectionRef("items", "")
		_, err := server.db.DescribeCollection(context.Background(), &ref)
		if !errors.Is(err, ErrMetadataUnavailable) {
			t.Fatalf("error = %v, want ErrMetadataUnavailable", err)
		}
		if got := server.hits.Load(); got != 1 {
			t.Fatalf("requests = %d, want 1 (only the root OpenAPI request)", got)
		}
	})
}

func TestMetadataOpenAPIComponents(t *testing.T) {
	body := `{
		"openapi":"3.0.0",
		"components":{"schemas":{"items":{"type":"object","properties":{"id":{"type":"integer","format":"int64"}}}}},
		"paths":{}
	}`
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, body) })
	collections, err := server.db.ListCollections(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(collections) != 1 || collections[0].Name() != "items" {
		t.Fatalf("collections = %#v", collections)
	}
	definition, err := server.db.DescribeCollection(context.Background(), &collections[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Fields) != 1 || definition.Fields[0].Type != dbschema.Int {
		t.Fatalf("fields = %#v", definition.Fields)
	}
}

func TestMetadataAfterClose(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, openAPIFixture)
	})
	if err := server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListCollections = %v, want ErrClosed", err)
	}
}

func TestMetadataUnsafeHeadersAreNotRewrittenAsUnavailable(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a metadata request was sent with unsafe headers")
		writeJSON(w, openAPIFixture)
	}, func(c *Config) {
		c.Headers = func(context.Context) (http.Header, error) {
			return http.Header{"X-Test": {"a\r\nb"}}, nil
		}
	})
	if _, err := server.db.ListCollections(context.Background(), nil); !errors.Is(err, ErrUnsafeHeader) {
		t.Fatalf("ListCollections = %v, want ErrUnsafeHeader", err)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}
