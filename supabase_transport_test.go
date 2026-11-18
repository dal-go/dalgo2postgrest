package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// TestTransportCancellationStopsBeforeRows is a hermetic transport unit proof,
// not a provider proof: it runs against this test's own httptest stub and never
// starts or contacts the Supabase-style fixture. It shows that cancelling the
// read context tears down the in-flight HTTP exchange before the adapter can
// deliver a row.
//
// The provider-backed counterpart is
// TestSupabaseIntegration_ProviderCancellationStopsBeforeRows, which forwards a
// bounded real request to the owned Kong/PostgREST fixture and is gated on
// SUPABASE_TEST_KONG_URL.
func TestTransportCancellationStopsBeforeRows(t *testing.T) {
	accepted := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case accepted <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}))
	defer stub.Close()

	db := supabaseCancellationDB(t, stub.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		Limit(5).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	type nextResult struct {
		rec record.Record
		err error
	}
	done := make(chan nextResult, 1)
	go func() {
		rec, nextErr := reader.Next()
		done <- nextResult{rec: rec, err: nextErr}
	}()
	select {
	case <-accepted:
	case <-time.After(10 * time.Second):
		t.Fatal("the owned stub never accepted the request")
	}
	cancel()
	select {
	case result := <-done:
		if result.rec != nil {
			t.Fatalf("the cancelled read delivered a row: %#v", result.rec)
		}
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("Next after cancellation = %v, want context.Canceled", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled read did not return")
	}
	select {
	case <-disconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the owned stub never observed the disconnect")
	}
}

func supabaseCancellationDB(t *testing.T, endpoint string) *Database {
	t.Helper()
	raw, err := NewDB(context.Background(), Config{
		Endpoint:              endpoint + "/rest/v1",
		IDColumn:              "record_id",
		AllowInsecureLoopback: true,
		Headers: func(context.Context) (http.Header, error) {
			return http.Header{
				"apikey":        {"unit-test-key"},
				"Authorization": {"Bearer unit-test-token"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	db, ok := raw.(*Database)
	if !ok {
		t.Fatalf("NewDB returned %T, want *Database", raw)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// A syntactically supported projection creates a lazy reader. A provider's
// native missing-column failure belongs to Next, with typed/redacted HTTP400.
func TestSupabaseFixtureBadProjectionDefersHTTP400UntilNext(t *testing.T) {
	const bodySecret = "synthetic-private-error-body"
	var requests atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/rest/v1/records" || r.URL.Query().Get("select") != "record_id,absent_column" {
			t.Errorf("unexpected native projection request path=%q select=%q", r.URL.Path, r.URL.Query().Get("select"))
		}
		if r.Header.Get("apikey") != "unit-test-key" || r.Header.Get("Authorization") != "Bearer unit-test-token" {
			t.Error("projection request lost its credential headers")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"` + bodySecret + `","details":"unit-test-token"}`))
	}))
	defer stub.Close()
	db := supabaseCancellationDB(t, stub.URL)
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().Limit(1).
		SelectColumns(dal.Column{Expression: dal.Field("record_id")}, dal.Column{Expression: dal.Field("absent_column")})
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("construct lazy reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if got := requests.Load(); got != 0 {
		t.Fatalf("reader construction sent %d requests, want none", got)
	}
	rec, err := reader.Next()
	if rec != nil || err == nil {
		t.Fatalf("Next = %#v, %v; want no row and HTTP400", rec, err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("Next error = %v, want typed HTTP400", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("native projection400 classified as unauthorized: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("Next sent %d requests, want one", got)
	}
	assertNoSecretInError(t, err, bodySecret, "unit-test-token", "unit-test-key", stub.URL)
}
