package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

func TestReadOnlySurface(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a refused write performed HTTP I/O")
		writeJSON(w, "[]")
	})
	if _, ok := any(server.db).(dal.WriteSession); ok {
		t.Fatal("the adapter exposes dal.WriteSession")
	}
	called := false
	err := server.db.RunReadwriteTransaction(context.Background(), func(context.Context, dal.ReadwriteTransaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("RunReadwriteTransaction = %v, want ErrNotSupported", err)
	}
	if called {
		t.Fatal("the read-write transaction callback was invoked")
	}
	called = false
	err = server.db.RunReadonlyTransaction(context.Background(), func(context.Context, dal.ReadTransaction) error {
		called = true
		return nil
	})
	if !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("RunReadonlyTransaction = %v, want ErrNotSupported", err)
	}
	if called {
		t.Fatal("the read-only transaction callback was invoked")
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}
