package dalgo2postgrest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// These tests run against the owned Supabase-style fixture started by
// scripts/check-supabase-rest.sh: PostgreSQL 18 and PostgREST 16.5 behind a
// Kong 3.9.3 gateway whose declarative configuration refuses the exact
// OpenAPI root to the public fixture key while forwarding a scoped user bearer
// to PostgREST. They are opt-in: without SUPABASE_TEST_KONG_URL the normal
// hermetic suite skips them and never starts a container.

type supabaseFixture struct {
	base          string
	apiKey        string
	bearer        string
	expiredBearer string
	emptySubB     string
	noaccessB     string
	badAPIKey     string
	badBearer     string
}

func supabaseFixtureFromEnv(t *testing.T) supabaseFixture {
	t.Helper()
	base := os.Getenv("SUPABASE_TEST_KONG_URL")
	if base == "" {
		t.Skip("set SUPABASE_TEST_KONG_URL to run against the owned Supabase-style fixture")
	}
	required := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s must be set when SUPABASE_TEST_KONG_URL is set", name)
		}
		return value
	}
	return supabaseFixture{
		base:          strings.TrimRight(base, "/"),
		apiKey:        required("SUPABASE_TEST_API_KEY"),
		bearer:        required("SUPABASE_TEST_BEARER"),
		expiredBearer: required("SUPABASE_TEST_EXPIRED_BEARER"),
		emptySubB:     required("SUPABASE_TEST_EMPTY_SUB_BEARER"),
		noaccessB:     required("SUPABASE_TEST_NOACCESS_BEARER"),
		badAPIKey:     required("SUPABASE_TEST_BAD_API_KEY"),
		badBearer:     required("SUPABASE_TEST_BAD_BEARER"),
	}
}

func supabaseDB(t *testing.T, base, apiKey, bearer string, mutate ...func(*Config)) *Database {
	t.Helper()
	cfg := Config{
		Endpoint:              strings.TrimRight(base, "/") + "/rest/v1",
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
	db, err := NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	database, ok := db.(*Database)
	if !ok {
		t.Fatalf("NewDB returned %T, want *Database", db)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func supabaseRaw(t *testing.T, method, url, apiKey, bearer, body string) (int, []byte) {
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
	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
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

func assertNoSecretInError(t *testing.T, err error, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked a credential or endpoint: %v", err)
		}
	}
}

func TestSupabaseIntegration_KnownRelationPreservesNativeValues(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)
	ctx := context.Background()

	first := record.NewRecordWithData(record.NewKeyWithID("records", int64(1)), map[string]any{})
	if err := db.Get(ctx, first); err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	data := first.Data().(map[string]any)
	if data["record_id"] != int64(1) || data["title"] != "Record 1" {
		t.Fatalf("native identity/title = %#v", data)
	}
	if data["big_number"] != int64(9007199254740993) {
		t.Fatalf("big_number = %#v (%T), want the exact 2^53+1 int64", data["big_number"], data["big_number"])
	}
	assertNumber(t, data["amount"], "0.125")
	tags, ok := data["tags"].([]any)
	if !ok || len(tags) != 1 || tags[0] != "alpha" {
		t.Fatalf("tags = %#v, want a native string array", data["tags"])
	}
	payload, ok := data["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %#v, want a native JSON object", data["payload"])
	}
	if payload["sequence"] != int64(1) {
		t.Fatalf("payload.sequence = %#v, want int64(1)", payload["sequence"])
	}
	if nested, ok := payload["nested"].(map[string]any); !ok || nested["type"] != "value" {
		t.Fatalf("payload.nested = %#v", payload["nested"])
	}

	// SQL NULL, empty string and a non-empty value stay distinct.
	nullRecord := record.NewRecordWithData(record.NewKeyWithID("records", int64(5)), map[string]any{})
	if err := db.Get(ctx, nullRecord); err != nil {
		t.Fatalf("Get(5): %v", err)
	}
	if value, present := nullRecord.Data().(map[string]any)["nullable"]; !present || value != nil {
		t.Fatalf("row 5 nullable = %#v, want present SQL NULL", value)
	}
	emptyRecord := record.NewRecordWithData(record.NewKeyWithID("records", int64(7)), map[string]any{})
	if err := db.Get(ctx, emptyRecord); err != nil {
		t.Fatalf("Get(7): %v", err)
	}
	emptyData := emptyRecord.Data().(map[string]any)
	if value, present := emptyData["nullable"]; !present || value != "" {
		t.Fatalf("row 7 nullable = %#v, want present empty string", value)
	}
	if value, present := emptyData["tags"]; !present || value != nil {
		t.Fatalf("row 7 tags = %#v, want present SQL NULL array", value)
	}
	bothTags := record.NewRecordWithData(record.NewKeyWithID("records", int64(4)), map[string]any{})
	if err := db.Get(ctx, bothTags); err != nil {
		t.Fatalf("Get(4): %v", err)
	}
	if tags, ok := bothTags.Data().(map[string]any)["tags"].([]any); !ok || len(tags) != 2 {
		t.Fatalf("row 4 tags = %#v, want two native elements", bothTags.Data().(map[string]any)["tags"])
	}

	second := record.NewRecordWithData(record.NewKeyWithID("records", int64(2)), map[string]any{})
	if err := db.Get(ctx, second); err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	assertNumber(t, second.Data().(map[string]any)["amount"], "0.250")

	found, err := db.Exists(ctx, record.NewKeyWithID("records", int64(1)))
	if err != nil || !found {
		t.Fatalf("Exists(1) = %v, %v", found, err)
	}
	missing := record.NewRecordWithData(record.NewKeyWithID("records", int64(999_999)), map[string]any{})
	if err = db.Get(ctx, missing); !record.IsNotFound(err) {
		t.Fatalf("Get(absurd) = %v, want not found", err)
	}
}

func TestSupabaseIntegration_ServerCapIsContinuedByPages(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	recorder := newRecordingTransport()
	db := supabaseDB(t, f.base, f.apiKey, f.bearer, func(cfg *Config) {
		cfg.HTTPClient = &http.Client{Transport: recorder}
	})
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		OrderBy(dal.AscendingField("record_id")).
		Limit(100).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 100 {
		t.Fatalf("read %d rows, want 100 across server-capped pages", len(records))
	}
	for i, rec := range records {
		if rec.Key().ID != int64(i+1) {
			t.Fatalf("row %d key = %#v, want %d", i, rec.Key().ID, i+1)
		}
	}
	requests, ranges := recorder.snapshot()
	// Three physical requests: 37 + 37 + 26 rows. A short first page must not
	// be treated as the end of the relation.
	if requests != 3 {
		t.Fatalf("client made %d requests for 100 rows at server cap 37, want 3 (ranges %v)", requests, ranges)
	}
	if len(ranges) != requests {
		t.Fatalf("recorded %d Content-Range headers for %d requests", len(ranges), requests)
	}
	offsets := []string{"0-", "37-", "74-"}
	for i, value := range ranges {
		if value == "" {
			// A missing header is tolerated by the adapter; its absence cannot
			// silently truncate because another request is still required.
			continue
		}
		if !strings.HasPrefix(value, offsets[i]) {
			t.Fatalf("Content-Range %d = %q, want it to start with %q", i, value, offsets[i])
		}
		if !strings.HasSuffix(value, "/*") {
			t.Fatalf("Content-Range %d = %q, want an unknown total because the pager does not request count", i, value)
		}
	}
}

func TestSupabaseIntegration_RecordsetKeepsTypedColumns(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		OrderBy(dal.AscendingField("record_id")).
		Limit(5).
		SelectColumns(
			dal.Column{Expression: dal.Field("record_id")},
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Expression: dal.Field("amount")},
		)
	reader, err := db.ExecuteQueryToRecordsetReader(context.Background(), query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsetReader: %v", err)
	}
	rs := reader.Recordset()
	if rs.ColumnsCount() != 3 {
		t.Fatalf("recordset has %d columns, want 3", rs.ColumnsCount())
	}
	names := make([]string, 0, rs.ColumnsCount())
	for _, column := range rs.Columns() {
		names = append(names, column.Name())
	}
	if got := strings.Join(names, ","); got != "record_id,title,amount" {
		t.Fatalf("recordset columns = %s", got)
	}
	rows := 0
	for {
		row, gotRS, nextErr := reader.Next()
		if errors.Is(nextErr, dal.ErrNoMoreRecords) {
			break
		}
		if nextErr != nil {
			t.Fatalf("recordset Next: %v", nextErr)
		}
		id, getErr := row.GetValueByName("record_id", gotRS)
		if getErr != nil {
			t.Fatalf("recordset record_id: %v", getErr)
		}
		if id != int64(rows+1) {
			t.Fatalf("recordset row %d id = %#v, want int64(%d)", rows, id, rows+1)
		}
		rows++
	}
	if rows != 5 {
		t.Fatalf("recordset returned %d rows, want 5", rows)
	}
}

func TestSupabaseIntegration_MetadataRootRefusalIsTypedNotEmpty(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)
	ctx := context.Background()

	collections, err := db.ListCollections(ctx, nil)
	if err == nil {
		t.Fatalf("ListCollections through the forbidden root returned %d collections, want a typed refusal", len(collections))
	}
	if !errors.Is(err, ErrMetadataUnauthorized) {
		t.Fatalf("ListCollections = %v, want ErrMetadataUnauthorized", err)
	}
	var metadataErr *MetadataError
	if !errors.As(err, &metadataErr) || metadataErr.StatusCode != http.StatusForbidden {
		t.Fatalf("ListCollections MetadataError = %#v, want HTTP 403", metadataErr)
	}
	assertNoSecretInError(t, err, f.bearer, f.apiKey, f.base)

	ref := dal.NewRootCollectionRef("records", "")
	definition, err := db.DescribeCollection(ctx, &ref)
	if err == nil {
		t.Fatalf("DescribeCollection through the forbidden root returned %#v, want a typed refusal", definition)
	}
	if !errors.Is(err, ErrMetadataUnauthorized) {
		t.Fatalf("DescribeCollection = %v, want ErrMetadataUnauthorized", err)
	}

	// The known relation still reads: a metadata refusal is not an empty
	// schema and does not disable relation reads.
	first := record.NewRecordWithData(record.NewKeyWithID("records", int64(2)), map[string]any{})
	if err = db.Get(ctx, first); err != nil {
		t.Fatalf("Get(2) after metadata refusal: %v", err)
	}
}

func TestSupabaseIntegration_AuthRefusalsAreSafe(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		apiKey string
		bearer string
	}{
		{"bad-apikey", f.badAPIKey, f.bearer},
		{"missing-apikey", "", f.bearer},
		{"expired-bearer", f.apiKey, f.expiredBearer},
		{"wrongly-signed-bearer", f.apiKey, f.badBearer},
		{"missing-bearer", f.apiKey, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db := supabaseDB(t, f.base, testCase.apiKey, testCase.bearer)
			_, err := db.Exists(ctx, record.NewKeyWithID("records", int64(1)))
			if err == nil {
				t.Fatal("the refusal case succeeded")
			}
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("error = %v, want *HTTPError", err)
			}
			if httpErr.StatusCode != http.StatusUnauthorized && httpErr.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 401 or 403", httpErr.StatusCode)
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("error = %v, want errors.Is(ErrUnauthorized)", err)
			}
			assertNoSecretInError(t, err, testCase.bearer, testCase.apiKey, f.base)
		})
	}
}

func TestSupabaseIntegration_RLSHidesOwnerRowsAndEmptySubject(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	ctx := context.Background()
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)

	// The other owner's row is present in the table (the acceptance script
	// counts it externally) and invisible through this token.
	hidden := record.NewRecordWithData(record.NewKeyWithID("records", int64(9001)), map[string]any{})
	if err := db.Get(ctx, hidden); !record.IsNotFound(err) {
		t.Fatalf("Get(9001) = %v, want not found through RLS", err)
	}
	own := record.NewRecordWithData(record.NewKeyWithID("records", int64(1)), map[string]any{})
	if err := db.Get(ctx, own); err != nil {
		t.Fatalf("Get(1): %v", err)
	}

	// Raw HTTP confirms the same premise the adapter observes: 200 with an
	// empty array, not an error and not a fabricated schema.
	status, body := supabaseRaw(t, http.MethodGet,
		f.base+"/rest/v1/records?select=record_id&record_id=eq.9001", f.apiKey, f.bearer, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("raw hidden-row read = HTTP %d %s, want 200 []", status, strings.TrimSpace(string(body)))
	}

	emptyDB := supabaseDB(t, f.base, f.apiKey, f.emptySubB)
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		Limit(10).
		SelectIntoRecord(nil)
	reader, err := emptyDB.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		t.Fatalf("empty-subject query: %v", err)
	}
	if rows := consumeRecords(t, reader); len(rows) != 0 {
		t.Fatalf("empty subject read %d rows, want none", len(rows))
	}
	status, body = supabaseRaw(t, http.MethodGet,
		f.base+"/rest/v1/records?select=record_id&limit=10", f.apiKey, f.emptySubB, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("raw empty-subject read = HTTP %d %s, want 200 []", status, strings.TrimSpace(string(body)))
	}
}

func TestSupabaseIntegration_DeniedGrantAndWriteLeaveNoRow(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	ctx := context.Background()

	denied := supabaseDB(t, f.base, f.apiKey, f.noaccessB)
	_, err := denied.Exists(ctx, record.NewKeyWithID("records", int64(1)))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("noaccess Exists = %v, want ErrUnauthorized", err)
	}
	status, body := supabaseRaw(t, http.MethodGet,
		f.base+"/rest/v1/records?select=record_id&limit=1", f.apiKey, f.noaccessB, "")
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("raw noaccess read = HTTP %d, want 401/403", status)
	}
	if len(body) != 0 && strings.Contains(string(body), `"record_id"`) {
		t.Fatalf("raw noaccess read returned rows: %s", body)
	}

	// A real write refusal, then an adapter-level and raw-level row-absence
	// probe. The acceptance script also counts the probe row in PostgreSQL.
	writeBody := `{"record_id":990001,"title":"adapter write probe","amount":1,"big_number":1,"owner_sub":"fixture-user","payload":{},"created_at":"2026-01-01T00:00:00Z"}`
	status, _ = supabaseRaw(t, http.MethodPost, f.base+"/rest/v1/records", f.apiKey, f.bearer, writeBody)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("raw write probe = HTTP %d, want 401/403", status)
	}
	// The row-absence probe uses the fixture user, so it is about the refused
	// write and not about the noaccess role.
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)
	probe := record.NewRecordWithData(record.NewKeyWithID("records", int64(990001)), map[string]any{})
	if err = db.Get(ctx, probe); !record.IsNotFound(err) {
		t.Fatalf("write probe Get = %v, want not found", err)
	}
}

func TestSupabaseIntegration_BadProjectionIsNative400(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	db := supabaseDB(t, f.base, f.apiKey, f.bearer)
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		Limit(1).
		SelectColumns(
			dal.Column{Expression: dal.Field("record_id")},
			dal.Column{Expression: dal.Field("absent_column")},
		)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("construct lazy projection reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	if rec, nextErr := reader.Next(); nextErr == nil {
		t.Fatalf("a projection with an absent column returned a row: %#v", rec)
	} else {
		err = nextErr
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("projection error = %v, want a native HTTP 400", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a projection 400 must not be reported as unauthorized: %v", err)
	}
	assertNoSecretInError(t, err, f.bearer, f.apiKey, f.base)
}

// TestSupabaseIntegration_ProviderCancellationStopsBeforeRows proves cancellation
// against the owned provider, not against an independent stub: it starts a
// bounded in-process proxy that forwards the adapter's request to the real
// Kong/PostgREST fixture, waits for the provider's HTTP 200, then holds the
// response until the adapter disconnects. The adapter must return
// context.Canceled without delivering a row, and the proxy must observe the
// disconnect. The hermetic transport-only proof of the same adapter behavior is
// TestTransportCancellationStopsBeforeRows in supabase_transport_test.go.
func TestSupabaseIntegration_ProviderCancellationStopsBeforeRows(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	accepted := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	upstreamFailure := make(chan error, 1)
	upstream := strings.TrimRight(f.base, "/")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := upstream + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		request, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
		if err != nil {
			select {
			case upstreamFailure <- err:
			default:
			}
			return
		}
		request.Header = r.Header.Clone()
		response, err := http.DefaultTransport.RoundTrip(request)
		if err != nil {
			select {
			case upstreamFailure <- err:
			default:
			}
			return
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			select {
			case upstreamFailure <- fmt.Errorf("the owned fixture answered HTTP %d", response.StatusCode):
			default:
			}
			return
		}
		select {
		case accepted <- struct{}{}:
		default:
		}
		// Hold the provider's real response until the adapter disconnects. No
		// header or row reaches it before the bounded context is cancelled.
		<-r.Context().Done()
		select {
		case disconnected <- struct{}{}:
		default:
		}
	}))
	defer proxy.Close()

	db := supabaseDB(t, proxy.URL, f.apiKey, f.bearer)
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
	case failure := <-upstreamFailure:
		t.Fatalf("the bounded provider proxy could not reach the owned fixture: %v", failure)
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded provider proxy never accepted the request")
	}
	cancel()
	select {
	case result := <-done:
		if result.rec != nil {
			t.Fatalf("the cancelled provider read delivered a row: %#v", result.rec)
		}
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("Next after cancellation = %v, want context.Canceled", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled provider read did not return")
	}
	select {
	case <-disconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("the bounded provider proxy never observed the disconnect")
	}
}

func TestSupabaseIntegration_HeaderRefreshAndClosedHandle(t *testing.T) {
	f := supabaseFixtureFromEnv(t)
	var headersCalls atomic.Int32
	db := supabaseDB(t, f.base, f.apiKey, f.bearer, func(cfg *Config) {
		cfg.Headers = func(context.Context) (http.Header, error) {
			headersCalls.Add(1)
			headers := http.Header{}
			headers.Set("apikey", f.apiKey)
			headers.Set("Authorization", "Bearer "+f.bearer)
			return headers, nil
		}
	})
	query := dal.From(dal.NewRootCollectionRef("records", "")).NewQuery().
		OrderBy(dal.AscendingField("record_id")).
		Limit(100).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatalf("ExecuteQueryToRecordsReader: %v", err)
	}
	if rows := consumeRecords(t, reader); len(rows) != 100 {
		t.Fatalf("read %d rows, want 100", len(rows))
	}
	// One credential-provider call per physical request: the credential is not
	// cached across pages.
	if calls := headersCalls.Load(); calls < 3 {
		t.Fatalf("header provider ran %d times for 3 server-capped pages, want at least 3", calls)
	}

	if err = db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed := record.NewRecordWithData(record.NewKeyWithID("records", int64(1)), map[string]any{})
	if err = db.Get(context.Background(), closed); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close = %v, want ErrClosed", err)
	}
}
