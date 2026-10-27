package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func TestMalformedResponsesAreRejected(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		status  int
		headers map[string]string
	}{
		{name: "invalid JSON", body: `not-json`, status: 200},
		{name: "object instead of array", body: `{"id":1}`, status: 200},
		{name: "array element not object", body: `[1]`, status: 200},
		{name: "trailing JSON value", body: `[] []`, status: 200},
		{name: "truncated JSON", body: `[{"id":1`, status: 200},
		{name: "empty body", body: ``, status: 200},
		{name: "too many rows", body: `[{"id":1},{"id":2}]`, status: 200},
		{name: "missing id", body: `[{"title":"one"}]`, status: 200},
		{name: "wrong content type", body: `[{"id":1}]`, status: 200, headers: map[string]string{"Content-Type": "text/html"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
				for name, value := range test.headers {
					w.Header().Set(name, value)
				}
				if test.headers == nil {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			})
			_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
			if strings.Contains(err.Error(), test.body) && test.body != "" {
				t.Fatalf("error leaked the body: %v", err)
			}
		})
	}
}

func TestResponseByteBudget(t *testing.T) {
	t.Run("content length", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			_, _ = w.Write([]byte("[]"))
		}, func(c *Config) { c.MaxResponseBytes = 10 })
		_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("error = %v, want ErrResponseTooLarge", err)
		}
	})

	t.Run("chunked body", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(`[{"id":1,"title":"` + strings.Repeat("x", 200) + `"}]`))
		}, func(c *Config) { c.MaxResponseBytes = 10 })
		_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("error = %v, want ErrResponseTooLarge", err)
		}
	})

	t.Run("total across pages", func(t *testing.T) {
		var hits int
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			hits++
			writeJSON(w, `[{"id":1,"title":"`+strings.Repeat("x", 30)+`"}]`)
		}, func(c *Config) {
			c.MaxPageSize = 1
			c.MaxRequests = 3
			c.MaxResponseBytes = 60
		})
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(3).SelectIntoRecord(nil)
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		var read int
		for {
			_, err = reader.Next()
			if errors.Is(err, dal.ErrNoMoreRecords) {
				t.Fatal("reader ended successfully despite the total byte budget")
			}
			if err != nil {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("error = %v, want ErrResponseTooLarge", err)
				}
				break
			}
			read++
		}
		if read != 1 || hits != 2 {
			t.Fatalf("read = %d, hits = %d; want 1 row then a budget failure on page 2", read, hits)
		}
	})
}

func TestTruncatedBodyIsRejected(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`[{"id":1}]`))
	})
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestGetChecksReturnedIdentity(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2,"title":"wrong"}]`)
	})
	rec := record.NewRecordWithData(record.NewKeyWithID("items", 1), map[string]any{})
	err := server.db.Get(context.Background(), rec)
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}

func TestDeepJSONIsRejected(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(`[{"id":1,"x":`)
	for i := 0; i < 40; i++ {
		builder.WriteByte('[')
	}
	builder.WriteString("1")
	for i := 0; i < 40; i++ {
		builder.WriteByte(']')
	}
	builder.WriteString(`}]`)
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, builder.String())
	})
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("error = %v, want ErrInvalidResponse", err)
	}
}
