package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func TestHTTPStatusErrorsDoNotLeakBodies(t *testing.T) {
	const secret = "super-secret-row-value"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"` + secret + `","details":{"token":"` + secret + `"}}`))
			})
			found, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
			if err == nil {
				t.Fatal("expected an error")
			}
			if found {
				t.Fatal("Exists returned true on an HTTP error")
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
				t.Fatalf("error = %v, want HTTPError %d", err, status)
			}
			if errors.Is(err, ErrUnauthorized) != (status == 401 || status == 403) {
				t.Fatalf("ErrUnauthorized match for %d = %v", status, errors.Is(err, ErrUnauthorized))
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), server.server.URL) {
				t.Fatalf("error leaked a body or URL: %v", err)
			}
		})
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		writeJSON(w, `[{"id":1}]`)
	}))
	defer target.Close()

	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}, func(c *Config) {
		c.HTTPClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	})
	found, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if found || !errors.Is(err, ErrRedirectRefused) {
		t.Fatalf("Exists = %v, %v; want ErrRedirectRefused", found, err)
	}
	if targetHits != 0 {
		t.Fatalf("redirect target was called %d times", targetHits)
	}
}

func TestHeaderProviderFailuresAreRedacted(t *testing.T) {
	const secret = "header-secret-token"
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was sent despite a header failure")
		writeJSON(w, "[]")
	}, func(c *Config) {
		c.Headers = func(context.Context) (http.Header, error) {
			return nil, errors.New(secret)
		}
	})
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("error = %v, want ErrRequestFailed", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the provider error: %v", err)
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestUnsafeHeadersAreRefusedBeforeIO(t *testing.T) {
	cases := []http.Header{
		{"X-Test": {"a\r\nInjected: yes"}},
		{"Bad Header": {"value"}},
		{"X-Test": {"value\x00"}},
		{"Prefer": {"count=exact"}},
		{"Accept": {"text/csv"}},
		{"Accept-Profile": {"other"}},
		{"Content-Profile": {"other"}},
		{"Host": {"evil.example"}},
	}
	for _, headers := range cases {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("a request was sent despite an unsafe header")
			writeJSON(w, "[]")
		}, func(c *Config) {
			c.Headers = func(context.Context) (http.Header, error) { return headers, nil }
		})
		_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
		if !errors.Is(err, ErrUnsafeHeader) {
			t.Fatalf("headers %#v: error = %v, want ErrUnsafeHeader", headers, err)
		}
		if got := server.hits.Load(); got != 0 {
			t.Fatalf("requests = %d, want 0", got)
		}
	}
}

func TestHeaderProviderReceivesOperationContext(t *testing.T) {
	type contextKey struct{}
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer from-context" {
			t.Errorf("Authorization = %q", got)
		}
		writeJSON(w, "[]")
	}, func(c *Config) {
		c.Headers = func(ctx context.Context) (http.Header, error) {
			if ctx.Value(contextKey{}) != "marker" {
				t.Error("header provider did not receive the caller context")
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("header provider context has no operation deadline")
			}
			return http.Header{"Authorization": {"Bearer from-context"}}, nil
		}
	})
	ctx := context.WithValue(context.Background(), contextKey{}, "marker")
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	_ = consumeRecords(t, reader)
}

func TestAcceptProfileAndCustomHeaders(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept-Profile"); got != "app" {
			t.Errorf("Accept-Profile = %q", got)
		}
		if got := r.Header.Get("X-Client-Info"); got != "unit-test" {
			t.Errorf("X-Client-Info = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer unit-test" {
			t.Errorf("Authorization = %q", got)
		}
		writeJSON(w, "[]")
	}, func(c *Config) {
		c.Schema = "app"
		c.Headers = func(context.Context) (http.Header, error) {
			return http.Header{
				"Authorization": {"Bearer unit-test"},
				"X-Client-Info": {"unit-test"},
			}, nil
		}
	})
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransportFailureIsRedacted(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := closed.URL + "/rest/v1"
	closed.Close()
	db, err := NewDB(context.Background(), Config{
		Endpoint:              endpoint,
		AllowInsecureLoopback: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.(*Database).Close() }()
	_, err = db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, ErrRequestFailed) {
		t.Fatalf("error = %v, want ErrRequestFailed", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("error leaked the endpoint: %v", err)
	}
}
