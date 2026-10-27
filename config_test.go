package dalgo2postgrest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/dbschema"
)

func TestNewDBConfig(t *testing.T) {
	ctx := context.Background()
	valid := Config{Endpoint: "https://api.example.test/rest/v1"}
	db, err := NewDB(ctx, valid)
	if err != nil {
		t.Fatalf("NewDB(valid): %v", err)
	}
	database := db.(*Database)
	if database.cfg.idColumn != "id" {
		t.Fatalf("id column = %q, want id", database.cfg.idColumn)
	}
	if database.cfg.maxRows != defaultMaxRows || database.cfg.maxPageSize != defaultMaxPageSize {
		t.Fatalf("unexpected defaults: %#v", database.cfg)
	}
	if !db.SupportsConcurrentConnections() {
		t.Fatal("SupportsConcurrentConnections() = false, want true")
	}
	if got := db.Adapter().Name(); got != "dalgo2postgrest" {
		t.Fatalf("adapter = %q", got)
	}
	if got := db.Adapter().Version(); got != Version {
		t.Fatalf("adapter version = %q", got)
	}
	if !strings.Contains(db.ID(), "api.example.test") {
		t.Fatalf("ID() = %q", db.ID())
	}
	if _, ok := dal.As[dbschema.SchemaReader](db); !ok {
		t.Fatal("dal.As[dbschema.SchemaReader] failed")
	}
	if got, ok := dal.As[*Database](db); !ok || got != database {
		t.Fatal("dal.As[*Database] did not return the concrete handle")
	}
	if database.Unwrap() == nil {
		t.Fatal("Unwrap returned nil")
	}
	if err = database.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err = database.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err = database.ListCollections(ctx, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("ListCollections after Close = %v, want ErrClosed", err)
	}
	if _, err = database.ExecuteQueryToRecordsReader(ctx, dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("query after Close = %v, want ErrClosed", err)
	}
}

func TestNewDBRejectsUnsafeEndpoints(t *testing.T) {
	for _, test := range []struct {
		name   string
		config Config
	}{
		{"empty", Config{}},
		{"plain http", Config{Endpoint: "http://api.example.test"}},
		{"plain http loopback without opt in", Config{Endpoint: "http://127.0.0.1:3000"}},
		{"userinfo", Config{Endpoint: "https://user:pass@api.example.test"}},
		{"query", Config{Endpoint: "https://api.example.test/?apikey=secret"}},
		{"fragment", Config{Endpoint: "https://api.example.test/#secret"}},
		{"relative", Config{Endpoint: "/rest/v1"}},
		{"dot dot path", Config{Endpoint: "https://api.example.test/rest/../v1"}},
		{"encoded dot dot path", Config{Endpoint: "https://api.example.test/rest/%2e%2e/v1"}},
		{"bad id column", Config{Endpoint: "https://api.example.test", IDColumn: "id column"}},
		{"bad schema", Config{Endpoint: "https://api.example.test", Schema: "app;drop"}},
		{"negative rows", Config{Endpoint: "https://api.example.test", MaxRows: -1}},
		{"rows over cap", Config{Endpoint: "https://api.example.test", MaxRows: absoluteMaxRows + 1}},
		{"page over rows", Config{Endpoint: "https://api.example.test", MaxRows: 10, MaxPageSize: 11}},
		{"requests over cap", Config{Endpoint: "https://api.example.test", MaxRequests: absoluteMaxRequests + 1}},
		{"response too small", Config{Endpoint: "https://api.example.test", MaxResponseBytes: -1}},
		{"metadata too large", Config{Endpoint: "https://api.example.test", MaxMetadataBytes: absoluteMaxResponse + 1}},
		{"offset over cap", Config{Endpoint: "https://api.example.test", MaxOffset: absoluteMaxOffset + 1}},
		{"getmany over cap", Config{Endpoint: "https://api.example.test", MaxGetManyKeys: absoluteMaxGetManyKeys + 1}},
		{"timeout too small", Config{Endpoint: "https://api.example.test", Timeout: time.Microsecond}},
		{"timeout too large", Config{Endpoint: "https://api.example.test", Timeout: absoluteMaxTimeout + time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewDB(context.Background(), test.config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("NewDB error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestNewDBLoopbackDevelopmentEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://localhost:3000", "http://127.0.0.1:3000", "http://[::1]:3000"} {
		db, err := NewDB(context.Background(), Config{Endpoint: endpoint, AllowInsecureLoopback: true})
		if err != nil {
			t.Fatalf("NewDB(%q): %v", endpoint, err)
		}
		_ = db.(*Database).Close()
	}
}

func TestNewDBCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewDB(ctx, Config{Endpoint: "https://api.example.test"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("NewDB = %v, want context.Canceled", err)
	}
}

func TestNewDBClonesCallerClientWithoutFollowingRedirects(t *testing.T) {
	original := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return nil },
		Timeout:       time.Minute,
	}
	db, err := NewDB(context.Background(), Config{Endpoint: "https://api.example.test", HTTPClient: original})
	if err != nil {
		t.Fatal(err)
	}
	database := db.(*Database)
	if database.cfg.client == original {
		t.Fatal("NewDB reused the caller's client pointer")
	}
	if original.CheckRedirect == nil || original.Timeout != time.Minute {
		t.Fatal("NewDB mutated the caller's client")
	}
	if database.cfg.client.CheckRedirect == nil {
		t.Fatal("cloned client has no redirect policy")
	}
}

func TestConfigBoundsAreHonored(t *testing.T) {
	db, err := NewDB(context.Background(), Config{
		Endpoint:         "https://api.example.test",
		MaxRows:          7,
		MaxPageSize:      3,
		MaxRequests:      4,
		MaxResponseBytes: 1234,
		MaxMetadataBytes: 5678,
		MaxOffset:        9,
		MaxGetManyKeys:   5,
		Timeout:          250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := db.(*Database).cfg
	if cfg.maxRows != 7 || cfg.maxPageSize != 3 || cfg.maxRequests != 4 ||
		cfg.maxResponseBytes != 1234 || cfg.maxMetadataBytes != 5678 ||
		cfg.maxOffset != 9 || cfg.maxGetManyKeys != 5 || cfg.timeout != 250*time.Millisecond {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}
