package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func TestPreCanceledContextDoesNotSendRequest(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent with a canceled context")
		writeJSON(w, "[]")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	if _, err := server.db.ExecuteQueryToRecordsReader(ctx, query); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, err := server.db.Exists(ctx, record.NewKeyWithID("items", 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Exists error = %v, want context.Canceled", err)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestInflightCancellationStopsTheRequest(t *testing.T) {
	started := make(chan struct{})
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, nextErr := reader.Next()
		done <- nextErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after cancellation")
	}
	_ = reader.Close()
}

func TestOperationDeadlineCoversHTTP(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}, func(c *Config) { c.Timeout = 100 * time.Millisecond })
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = reader.Next()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("deadline did not stop the read promptly: %v", elapsed)
	}
}

func TestOperationDeadlineCoversHeaderProvider(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent after the header provider deadline")
		writeJSON(w, "[]")
	}, func(c *Config) {
		c.Timeout = 100 * time.Millisecond
		c.Headers = func(ctx context.Context) (http.Header, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestOperationDeadlineCoversANonCooperativeHeaderProvider(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent after the header provider deadline")
		writeJSON(w, "[]")
	}, func(c *Config) {
		c.Timeout = 50 * time.Millisecond
		c.Headers = func(context.Context) (http.Header, error) {
			time.Sleep(500 * time.Millisecond)
			return http.Header{"Authorization": {"Bearer late"}}, nil
		}
	})
	start := time.Now()
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("header provider deadline was not enforced: %v", elapsed)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestReaderCloseCancelsAnInflightPage(t *testing.T) {
	started := make(chan struct{})
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, nextErr := reader.Next()
		done <- nextErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Next error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not stop after Close")
	}
}

func TestRecordsReaderStopsBufferedRowsOnCancel(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2},{"id":1}]`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	first, err := reader.Next()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if first.Key().ID != int64(2) {
		t.Fatalf("first id = %#v", first.Key().ID)
	}
	cancel()
	for i := 0; i < 2; i++ {
		if _, err = reader.Next(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Next after cancel = %v, want context.Canceled (call %d)", err, i+1)
		}
		if strings.Contains(err.Error(), server.server.URL) {
			t.Fatalf("cancellation error leaked the endpoint: %v", err)
		}
	}
	_ = reader.Close()
}

func TestRecordsReaderStopsBufferedRowsOnCloseDB(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2},{"id":1}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err = server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = reader.Next(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after Close = %v, want ErrClosed", err)
	}
	if _, err = reader.Next(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Next after Close = %v, want the same ErrClosed", err)
	}
	_ = reader.Close()
}

func TestRecordsetReaderStopsBufferedRowsOnCancel(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2},{"id":1}]`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecordset()
	reader, err := server.db.ExecuteQueryToRecordsetReader(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = reader.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	cancel()
	for i := 0; i < 2; i++ {
		if _, _, err = reader.Next(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Next after cancel = %v, want context.Canceled (call %d)", err, i+1)
		}
	}
	_ = reader.Close()
}

func TestRecordsetReaderStopsBufferedRowsOnCloseDB(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2},{"id":1}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecordset()
	reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = reader.Next(); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err = server.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reader.Next(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after Close = %v, want ErrClosed", err)
	}
	if _, _, err = reader.Next(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Next after Close = %v, want the same ErrClosed", err)
	}
	_ = reader.Close()
}

func TestRecordsetReaderCancellationDuringRequest(t *testing.T) {
	started := make(chan struct{})
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecordset()
	done := make(chan error, 1)
	go func() {
		_, err := server.db.ExecuteQueryToRecordsetReader(ctx, query)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recordset error = %v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), server.server.URL) {
			t.Fatalf("cancellation error leaked the endpoint: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recordset read did not stop after cancellation")
	}
}

func TestCancellationDuringBodyRead(t *testing.T) {
	for _, test := range []struct {
		name string
		read func(context.Context, *Database) error
	}{
		{"records", func(ctx context.Context, db *Database) error {
			query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
			reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
			if err != nil {
				return err
			}
			_, err = reader.Next()
			_ = reader.Close()
			return err
		}},
		{"recordset", func(ctx context.Context, db *Database) error {
			query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecordset()
			_, err := db.ExecuteQueryToRecordsetReader(ctx, query)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`[{"id":1,`))
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
			})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- test.read(ctx, server.db)
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("body read did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("body read did not stop after cancellation")
			}
		})
	}
}
