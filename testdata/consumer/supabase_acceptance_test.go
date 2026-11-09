package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	postgrest "github.com/dal-go/dalgo2postgrest"
	"github.com/dal-go/record"
)

// This module imports the published github.com/dal-go/dalgo2postgrest version
// pinned in go.mod. There is no replace directive and no reference to a local
// checkout, so these tests are a consumer proof of that public artifact.

type fixture struct {
	base          string
	apiKey        string
	bearer        string
	expiredBearer string
	noaccessB     string
	badAPIKey     string
}

func fixtureFromEnv(t *testing.T) fixture {
	t.Helper()
	base := os.Getenv("SUPABASE_TEST_KONG_URL")
	if base == "" {
		t.Skip("set SUPABASE_TEST_KONG_URL to run the public-module consumer acceptance")
	}
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s must be set when SUPABASE_TEST_KONG_URL is set", name)
		}
		return value
	}
	return fixture{
		base:          strings.TrimRight(base, "/"),
		apiKey:        required("SUPABASE_TEST_API_KEY"),
		bearer:        required("SUPABASE_TEST_BEARER"),
		expiredBearer: required("SUPABASE_TEST_EXPIRED_BEARER"),
		noaccessB:     required("SUPABASE_TEST_NOACCESS_BEARER"),
		badAPIKey:     required("SUPABASE_TEST_BAD_API_KEY"),
	}
}

func open(t *testing.T, f fixture, apiKey, bearer string, mutate ...func(*postgrest.Config)) *postgrest.Database {
	t.Helper()
	cfg := postgrest.Config{
		Endpoint:              f.base + "/rest/v1",
		IDColumn:              "record_id",
		Schema:                "app",
		AllowInsecureLoopback: true,
		Headers: func(context.Context) (http.Header, error) {
			headers := http.Header{}
			if apiKey != "" {
				headers.Set("apikey", apiKey)
			}
			if bearer != "" {
				headers.Set("Authorization", "Bearer "+bearer)
			}
			return headers, nil
		},
	}
	for _, apply := range mutate {
		apply(&cfg)
	}
	raw, err := postgrest.NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	db, ok := raw.(*postgrest.Database)
	if !ok {
		t.Fatalf("NewDB returned %T, want *postgrest.Database", raw)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func rawRequest(t *testing.T, method, url, apiKey, bearer, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if apiKey != "" {
		request.Header.Set("apikey", apiKey)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Prefer", "return=minimal")
	}
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("raw %s: %v", method, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("raw %s body: %v", method, err)
	}
	return response.StatusCode, payload
}

type countingTransport struct {
	base     http.RoundTripper
	mu       sync.Mutex
	requests int
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := c.base.RoundTrip(request)
	c.mu.Lock()
	c.requests++
	c.mu.Unlock()
	return response, err
}

func (c *countingTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests
}

func TestPublishedSDKReadsKnownRelationWithTypedValues(t *testing.T) {
	f := fixtureFromEnv(t)
	db := open(t, f, f.apiKey, f.bearer)
	ctx := context.Background()

	first := record.NewRecordWithData(record.NewKeyWithID("records", int64(1)), map[string]any{})
	if err := db.Get(ctx, first); err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	data := first.Data().(map[string]any)
	if data["title"] != "Record 1" || data["record_id"] != int64(1) {
		t.Fatalf("identity/title = %#v", data)
	}
	if data["big_number"] != int64(9007199254740993) {
		t.Fatalf("big_number = %#v (%T), want the exact 2^53+1 int64", data["big_number"], data["big_number"])
	}
	amount, ok := data["amount"].(json.Number)
	if !ok || amount.String() != "0.125" {
		t.Fatalf("amount = %#v (%T), want json.Number(0.125)", data["amount"], data["amount"])
	}
	if tags, ok := data["tags"].([]any); !ok || len(tags) != 1 || tags[0] != "alpha" {
		t.Fatalf("tags = %#v, want a native array", data["tags"])
	}
	if payload, ok := data["payload"].(map[string]any); !ok || payload["sequence"] != int64(1) {
		t.Fatalf("payload = %#v", data["payload"])
	}
	nullRecord := record.NewRecordWithData(record.NewKeyWithID("records", int64(5)), map[string]any{})
	if err := db.Get(ctx, nullRecord); err != nil {
		t.Fatalf("Get(5): %v", err)
	}
	if value, present := nullRecord.Data().(map[string]any)["nullable"]; !present || value != nil {
		t.Fatalf("nullable = %#v, want present SQL NULL", value)
	}
	hidden := record.NewRecordWithData(record.NewKeyWithID("records", int64(9001)), map[string]any{})
	if err := db.Get(ctx, hidden); !record.IsNotFound(err) {
		t.Fatalf("Get(9001) = %v, want not found through RLS", err)
	}
}

func TestPublishedSDKContinuesServerCappedPages(t *testing.T) {
	f := fixtureFromEnv(t)
	counter := &countingTransport{base: http.DefaultTransport}
	db := open(t, f, f.apiKey, f.bearer, func(cfg *postgrest.Config) {
		cfg.HTTPClient = &http.Client{Transport: counter}
	})
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		OrderBy(dal.AscendingField("record_id")).
		Limit(100).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	var ids []any
	for {
		rec, nextErr := reader.Next()
		if errors.Is(nextErr, dal.ErrNoMoreRecords) {
			break
		}
		if nextErr != nil {
			t.Fatalf("Next: %v", nextErr)
		}
		ids = append(ids, rec.Key().ID)
	}
	if len(ids) != 100 {
		t.Fatalf("read %d rows, want 100 across the 37-row server cap", len(ids))
	}
	if count := counter.count(); count != 3 {
		t.Fatalf("client made %d requests for 100 rows at server cap 37, want 3", count)
	}
	for i, id := range ids {
		if id != int64(i+1) {
			t.Fatalf("row %d id = %#v, want %d", i, id, i+1)
		}
	}
}

func TestPublishedSDKMetadataRefusalIsTyped(t *testing.T) {
	f := fixtureFromEnv(t)
	db := open(t, f, f.apiKey, f.bearer)
	collections, err := db.ListCollections(context.Background(), nil)
	if err == nil {
		t.Fatalf("ListCollections through the forbidden OpenAPI root returned %d collections", len(collections))
	}
	if !errors.Is(err, postgrest.ErrMetadataUnauthorized) {
		t.Fatalf("ListCollections = %v, want ErrMetadataUnauthorized", err)
	}
	var metadataErr *postgrest.MetadataError
	if !errors.As(err, &metadataErr) || metadataErr.StatusCode != http.StatusForbidden {
		t.Fatalf("MetadataError = %#v, want HTTP 403", metadataErr)
	}
	if strings.Contains(err.Error(), f.bearer) || strings.Contains(err.Error(), f.base) {
		t.Fatalf("metadata refusal leaked a credential or endpoint: %v", err)
	}
	ref := dal.NewRootCollectionRef("records", "")
	if _, err = db.DescribeCollection(context.Background(), &ref); !errors.Is(err, postgrest.ErrMetadataUnauthorized) {
		t.Fatalf("DescribeCollection = %v, want ErrMetadataUnauthorized", err)
	}
	// The known relation still reads while metadata is refused.
	rec := record.NewRecordWithData(record.NewKeyWithID("records", int64(2)), map[string]any{})
	if err = db.Get(context.Background(), rec); err != nil {
		t.Fatalf("Get(2) after metadata refusal: %v", err)
	}
}

func TestPublishedSDKAuthRefusalsAreSafe(t *testing.T) {
	f := fixtureFromEnv(t)
	cases := []struct {
		name   string
		apiKey string
		bearer string
	}{
		{"bad-apikey", f.badAPIKey, f.bearer},
		{"missing-apikey", "", f.bearer},
		{"expired-bearer", f.apiKey, f.expiredBearer},
		{"noaccess-role", f.apiKey, f.noaccessB},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := open(t, f, testCase.apiKey, testCase.bearer)
			_, err := db.Exists(context.Background(), record.NewKeyWithID("records", int64(1)))
			if err == nil {
				t.Fatal("the refusal case succeeded")
			}
			if !errors.Is(err, postgrest.ErrUnauthorized) {
				t.Fatalf("Exists = %v, want ErrUnauthorized", err)
			}
			for _, secret := range []string{testCase.bearer, testCase.apiKey, f.base} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("refusal leaked a credential or endpoint: %v", err)
				}
			}
		})
	}
}

func TestPublishedSDKRLSAndWriteAbsence(t *testing.T) {
	f := fixtureFromEnv(t)
	status, body := rawRequest(t, http.MethodGet,
		f.base+"/rest/v1/records?select=record_id&record_id=eq.9001", f.apiKey, f.bearer, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("raw hidden-row read = HTTP %d %s, want 200 []", status, strings.TrimSpace(string(body)))
	}
	writeBody := `{"record_id":990001,"title":"consumer write probe","amount":1,"big_number":1,"owner_sub":"fixture-user","payload":{},"created_at":"2026-01-01T00:00:00Z"}`
	status, _ = rawRequest(t, http.MethodPost, f.base+"/rest/v1/records", f.apiKey, f.bearer, writeBody)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("raw write probe = HTTP %d, want 401/403", status)
	}
	db := open(t, f, f.apiKey, f.bearer)
	probe := record.NewRecordWithData(record.NewKeyWithID("records", int64(990001)), map[string]any{})
	if err := db.Get(context.Background(), probe); !record.IsNotFound(err) {
		t.Fatalf("write probe Get = %v, want not found", err)
	}
}

func TestPublishedSDKProjectionIsExact(t *testing.T) {
	f := fixtureFromEnv(t)
	db := open(t, f, f.apiKey, f.bearer)
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		OrderBy(dal.AscendingField("record_id")).
		Limit(1).
		SelectColumns(
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Expression: dal.Field("amount")},
		)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	rec, err := reader.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if rec.Key().ID != int64(1) {
		t.Fatalf("record key = %#v, want the hidden identity 1", rec.Key().ID)
	}
	data := rec.Data().(map[string]any)
	if len(data) != 2 || data["title"] != "Record 1" {
		t.Fatalf("projection data = %#v, want exactly title and amount", data)
	}
	if _, leaked := data["record_id"]; leaked {
		t.Fatalf("projection leaked the identity column: %#v", data)
	}
}
