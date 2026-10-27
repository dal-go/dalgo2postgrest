package dalgo2postgrest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type testServer struct {
	db      *Database
	server  *httptest.Server
	hits    *atomic.Int32
	handler http.HandlerFunc
}

func newTestDB(t *testing.T, handler http.HandlerFunc, mutate ...func(*Config)) *testServer {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	cfg := Config{
		Endpoint:              server.URL + "/rest/v1",
		AllowInsecureLoopback: true,
		Headers: func(context.Context) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer unit-test"}}, nil
		},
	}
	for _, apply := range mutate {
		apply(&cfg)
	}
	db, err := NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	database, ok := db.(*Database)
	if !ok {
		t.Fatalf("NewDB returned %T, want *Database", db)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &testServer{db: database, server: server, hits: hits, handler: handler}
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, body)
}

// writePage writes a JSON page with a PostgREST Content-Range header. Use an
// empty contentRange to exercise a provider that omits the header.
func writePage(w http.ResponseWriter, body, contentRange string) {
	if contentRange != "" {
		w.Header().Set("Content-Range", contentRange)
	}
	writeJSON(w, body)
}

func jsonRows(rows string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, rows)
	}
}
