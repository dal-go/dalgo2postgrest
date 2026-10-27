package dalgo2postgrest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func integrationConfig(t *testing.T) (endpoint, secret string) {
	t.Helper()
	endpoint = os.Getenv("POSTGREST_TEST_URL")
	secret = os.Getenv("POSTGREST_TEST_JWT_SECRET")
	if endpoint == "" {
		t.Skip("set POSTGREST_TEST_URL to run against a PostgREST endpoint")
	}
	return endpoint, secret
}

func integrationToken(t *testing.T, secret, role string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"role": role,
		"iat":  time.Now().Add(-time.Minute).Unix(),
		"exp":  time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(header + "." + payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return header + "." + payload + "." + signature
}

func integrationDB(t *testing.T, endpoint, token string, clients ...*http.Client) *Database {
	t.Helper()
	return integrationDBConfigured(t, endpoint, token, nil, clients...)
}

func integrationDBConfigured(t *testing.T, endpoint, token string, mutate func(*Config), clients ...*http.Client) *Database {
	t.Helper()
	apiKey := os.Getenv("POSTGREST_TEST_API_KEY")
	cfg := Config{
		Endpoint:              endpoint,
		AllowInsecureLoopback: true,
		Headers: func(context.Context) (http.Header, error) {
			headers := http.Header{"Authorization": {"Bearer " + token}}
			if apiKey != "" {
				headers.Set("apikey", apiKey)
			}
			return headers, nil
		},
	}
	if len(clients) > 0 && clients[0] != nil {
		cfg.HTTPClient = clients[0]
	}
	if mutate != nil {
		mutate(&cfg)
	}
	db, err := NewDB(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	database := db.(*Database)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// recordingTransport counts real HTTP exchanges and keeps the Content-Range
// headers a real PostgREST server sends. It is used to prove that capped pages
// were continued, not treated as the end of the relation.
type recordingTransport struct {
	base          http.RoundTripper
	mu            sync.Mutex
	requests      int
	contentRanges []string
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{base: http.DefaultTransport}
}

func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	t.mu.Lock()
	t.requests++
	if response != nil {
		t.contentRanges = append(t.contentRanges, response.Header.Get("Content-Range"))
	}
	t.mu.Unlock()
	return response, err
}

func (t *recordingTransport) snapshot() (int, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests, append([]string(nil), t.contentRanges...)
}

func integrationReaderToken(t *testing.T) (string, string) {
	endpoint, secret := integrationConfig(t)
	if token := os.Getenv("POSTGREST_TEST_READER_TOKEN"); token != "" {
		return endpoint, token
	}
	if secret == "" {
		t.Skip("set POSTGREST_TEST_READER_TOKEN or POSTGREST_TEST_JWT_SECRET")
	}
	role := os.Getenv("POSTGREST_TEST_READER_ROLE")
	if role == "" {
		role = "app_reader"
	}
	return endpoint, integrationToken(t, secret, role)
}

func TestIntegration_ReadsSeededRelation(t *testing.T) {
	endpoint, token := integrationReaderToken(t)
	db := integrationDB(t, endpoint, token)
	ctx := context.Background()

	rec := record.NewRecordWithData(record.NewKeyWithID("items", int64(1)), map[string]any{})
	if err := db.Get(ctx, rec); err != nil {
		t.Fatalf("Get: %v", err)
	}
	data := rec.Data().(map[string]any)
	if data["title"] != "alpha" {
		t.Fatalf("title = %#v", data["title"])
	}
	if data["id"] != int64(1) || data["qty"] != int64(10) {
		t.Fatalf("native ids/ints = %#v", data)
	}
	if data["big_qty"] != int64(9007199254740993) {
		t.Fatalf("big_qty = %#v (%T), want exact int64", data["big_qty"], data["big_qty"])
	}
	assertNumber(t, data["price"], "12.50")
	if data["active"] != true {
		t.Fatalf("active = %#v", data["active"])
	}
	if _, ok := data["doc"].(map[string]any); !ok {
		t.Fatalf("doc = %#v", data["doc"])
	}

	bigDecimal := record.NewRecordWithData(record.NewKeyWithID("items", int64(2)), map[string]any{})
	if err := db.Get(ctx, bigDecimal); err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	assertNumber(t, bigDecimal.Data().(map[string]any)["price"], "9007199254740993.00")

	note := record.NewRecordWithData(record.NewKeyWithID("notes", "note-1"), map[string]any{})
	if err := db.Get(ctx, note); err != nil {
		t.Fatalf("Get(note-1): %v", err)
	}
	if note.Key().ID != "note-1" || note.Data().(map[string]any)["body"] != "first note" {
		t.Fatalf("string-keyed record = %#v, %#v", note.Key().ID, note.Data())
	}

	found, err := db.Exists(ctx, record.NewKeyWithID("items", int64(1)))
	if err != nil || !found {
		t.Fatalf("Exists(1) = %v, %v", found, err)
	}
	found, err = db.Exists(ctx, record.NewKeyWithID("items", int64(99)))
	if err != nil || found {
		t.Fatalf("Exists(99) = %v, %v", found, err)
	}
	missing := record.NewRecordWithData(record.NewKeyWithID("items", int64(99)), map[string]any{})
	if err = db.Get(ctx, missing); !record.IsNotFound(err) {
		t.Fatalf("Get(99) = %v, want not found", err)
	}
	if missing.Exists() {
		t.Fatal("missing record reports Exists()")
	}

	one := record.NewRecordWithData(record.NewKeyWithID("items", int64(1)), map[string]any{})
	two := record.NewRecordWithData(record.NewKeyWithID("items", int64(2)), map[string]any{})
	absent := record.NewRecordWithData(record.NewKeyWithID("items", int64(99)), map[string]any{})
	if err = db.GetMulti(ctx, []record.Record{one, two, absent}); err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if one.Data().(map[string]any)["title"] != "alpha" || two.Data().(map[string]any)["title"] != "beta" {
		t.Fatalf("GetMulti data = %#v / %#v", one.Data(), two.Data())
	}
	if absent.Exists() {
		t.Fatal("GetMulti absent record reports Exists()")
	}

	page := func(offset int) []record.Record {
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
			WhereField("qty", dal.GreaterOrEqual, 20).
			OrderBy(dal.DescendingField("qty")).
			Limit(2).
			Offset(offset).
			SelectColumns(dal.Column{Expression: dal.Field("title")})
		reader, queryErr := db.ExecuteQueryToRecordsReader(ctx, query)
		if queryErr != nil {
			t.Fatalf("page query: %v", queryErr)
		}
		return consumeRecords(t, reader)
	}
	firstPage := page(0)
	if len(firstPage) != 2 || firstPage[0].Key().ID != int64(4) || firstPage[1].Key().ID != int64(2) {
		t.Fatalf("first page keys = %#v", keysOf(firstPage))
	}
	if _, found := firstPage[0].Data().(map[string]any)["id"]; found {
		t.Fatal("projection leaked the id column")
	}
	secondPage := page(2)
	if len(secondPage) != 1 || secondPage[0].Key().ID != int64(3) {
		t.Fatalf("second page keys = %#v", keysOf(secondPage))
	}

	membership := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("id", dal.In, []int64{1, 3}).
		OrderBy(dal.AscendingField("id")).
		Limit(2).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(ctx, membership)
	if err != nil {
		t.Fatal(err)
	}
	members := consumeRecords(t, reader)
	if len(members) != 2 || members[0].Key().ID != int64(1) || members[1].Key().ID != int64(3) {
		t.Fatalf("membership keys = %#v", keysOf(members))
	}

	orQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		Where(dal.NewGroupCondition(dal.Or,
			dal.WhereField("id", dal.Equal, 1),
			dal.WhereField("qty", dal.Equal, 30),
		)).
		OrderBy(dal.AscendingField("id")).
		Limit(2).
		SelectIntoRecord(nil)
	reader, err = db.ExecuteQueryToRecordsReader(ctx, orQuery)
	if err != nil {
		t.Fatal(err)
	}
	orRecords := consumeRecords(t, reader)
	if len(orRecords) != 2 || orRecords[0].Key().ID != int64(1) || orRecords[1].Key().ID != int64(4) {
		t.Fatalf("OR keys = %#v", keysOf(orRecords))
	}

	recordsetQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("qty", dal.GreaterOrEqual, 20).
		OrderBy(dal.AscendingField("id")).
		Limit(10).
		SelectColumns(
			dal.Column{Expression: dal.Field("id")},
			dal.Column{Expression: dal.Field("title")},
			dal.Column{Expression: dal.Field("qty")},
		)
	rsReader, err := db.ExecuteQueryToRecordsetReader(ctx, recordsetQuery)
	if err != nil {
		t.Fatal(err)
	}
	rs := rsReader.Recordset()
	if rs.ColumnsCount() != 3 || rs.Columns()[0].Name() != "id" || rs.Columns()[1].Name() != "title" || rs.Columns()[2].Name() != "qty" {
		t.Fatalf("columns = %#v", rs.Columns())
	}
	var rsRows int
	for {
		row, gotRS, nextErr := rsReader.Next()
		if errors.Is(nextErr, dal.ErrNoMoreRecords) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		id, getErr := row.GetValueByName("id", gotRS)
		if getErr != nil || id == nil {
			t.Fatalf("recordset id = %#v, %v", id, getErr)
		}
		rsRows++
	}
	if rsRows != 3 {
		t.Fatalf("recordset rows = %d, want 3", rsRows)
	}

	collections, err := db.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	var itemsRef *dal.CollectionRef
	for i := range collections {
		if collections[i].Name() == "items" {
			itemsRef = &collections[i]
		}
	}
	if itemsRef == nil {
		t.Fatalf("items not in metadata: %#v", collections)
	}
	definition, err := db.DescribeCollection(ctx, itemsRef)
	if err != nil {
		t.Fatalf("DescribeCollection: %v", err)
	}
	if len(definition.PrimaryKey) != 1 || definition.PrimaryKey[0] != "id" {
		t.Fatalf("primary key = %#v", definition.PrimaryKey)
	}
	if len(definition.Fields) < 5 {
		t.Fatalf("fields = %#v", definition.Fields)
	}
	if _, err = db.ListIndexes(ctx, itemsRef); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("ListIndexes = %v, want ErrNotSupported", err)
	}

	if _, ok := any(db).(dal.WriteSession); ok {
		t.Fatal("the adapter exposes dal.WriteSession")
	}
	if err = db.RunReadwriteTransaction(ctx, func(context.Context, dal.ReadwriteTransaction) error { return nil }); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("RunReadwriteTransaction = %v", err)
	}

	aggregate := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		GroupBy(dal.Field("qty")).
		Limit(5).
		SelectColumns(dal.Count())
	if _, err = db.ExecuteQueryToRecordsReader(ctx, aggregate); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("aggregate fallback = %v, want ErrNotSupported", err)
	}

	unbounded := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().SelectIntoRecord(nil)
	if _, err = db.ExecuteQueryToRecordsReader(ctx, unbounded); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("unbounded query = %v, want ErrNotSupported", err)
	}
}

func TestIntegration_DeniedRole(t *testing.T) {
	endpoint, secret := integrationConfig(t)
	if secret == "" {
		t.Skip("set POSTGREST_TEST_JWT_SECRET to mint the local denied-role token")
	}
	role := os.Getenv("POSTGREST_TEST_NOACCESS_ROLE")
	if role == "" {
		role = "noaccess"
	}
	token := integrationToken(t, secret, role)
	db := integrationDB(t, endpoint, token)
	_, err := db.Exists(context.Background(), record.NewKeyWithID("items", int64(1)))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Exists = %v, want ErrUnauthorized", err)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || (httpErr.StatusCode != 401 && httpErr.StatusCode != 403) {
		t.Fatalf("HTTP error = %#v", httpErr)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("error leaked the token or endpoint: %v", err)
	}
}

func TestIntegration_InvalidJWT(t *testing.T) {
	endpoint, _ := integrationConfig(t)
	db := integrationDB(t, endpoint, "not-a-valid-jwt")
	_, err := db.Exists(context.Background(), record.NewKeyWithID("items", int64(1)))
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Exists = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), "not-a-valid-jwt") || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("error leaked the token or endpoint: %v", err)
	}
	_, err = db.ListCollections(context.Background(), nil)
	if !errors.Is(err, ErrMetadataUnauthorized) {
		t.Fatalf("ListCollections = %v, want ErrMetadataUnauthorized", err)
	}
}

func TestIntegration_NullEmptyStringAndNotNull(t *testing.T) {
	endpoint, token := integrationReaderToken(t)
	db := integrationDB(t, endpoint, token)
	ctx := context.Background()

	for _, test := range []struct {
		id   int64
		want any
	}{
		{1, ""},
		{2, nil},
		{3, "value"},
		{4, ""},
		{5, "other"},
		{6, nil},
	} {
		rec := record.NewRecordWithData(record.NewKeyWithID("labels", test.id), map[string]any{})
		if err := db.Get(ctx, rec); err != nil {
			t.Fatalf("Get(label %d): %v", test.id, err)
		}
		if got := rec.Data().(map[string]any)["name"]; got != test.want {
			t.Fatalf("label %d name = %#v (%T), want %#v", test.id, got, got, test.want)
		}
	}

	queryIDs := func(t *testing.T, where dal.Condition) []int64 {
		t.Helper()
		query := dal.From(dal.NewRootCollectionRef("labels", "")).NewQuery().
			Where(where).
			OrderBy(dal.AscendingField("id")).
			Limit(10).
			SelectIntoRecord(nil)
		reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		records := consumeRecords(t, reader)
		ids := make([]int64, len(records))
		for i, rec := range records {
			ids[i] = rec.Key().ID.(int64)
		}
		return ids
	}

	if got := queryIDs(t, dal.WhereField("name", dal.Equal, "")); fmt.Sprint(got) != "[1 4]" {
		t.Fatalf("empty string = %v, want [1 4] (not the NULL rows)", got)
	}
	if got := queryIDs(t, dal.NewIsNullCondition(dal.Field("name"))); fmt.Sprint(got) != "[2 6]" {
		t.Fatalf("IS NULL = %v, want [2 6]", got)
	}
	if got := queryIDs(t, dal.NewIsNotNullCondition(dal.Field("name"))); fmt.Sprint(got) != "[1 3 4 5]" {
		t.Fatalf("IS NOT NULL = %v, want [1 3 4 5]", got)
	}
	if got := queryIDs(t, dal.WhereField("name", dal.Equal, "value")); fmt.Sprint(got) != "[3]" {
		t.Fatalf("non-empty equality = %v, want [3]", got)
	}
	orNull := dal.NewGroupCondition(dal.Or,
		dal.NewIsNullCondition(dal.Field("name")),
		dal.WhereField("name", dal.Equal, ""),
	)
	if got := queryIDs(t, orNull); fmt.Sprint(got) != "[1 2 4 6]" {
		t.Fatalf("OR(IS NULL, empty string) = %v, want [1 2 4 6]", got)
	}

	// DALgo comparison with NULL is UNKNOWN; it must be refused, not turned into
	// a PostgREST is.null that would return the NULL rows.
	nullComparison := dal.From(dal.NewRootCollectionRef("labels", "")).NewQuery().
		WhereField("name", dal.Equal, nil).
		Limit(1).
		SelectIntoRecord(nil)
	if _, err := db.ExecuteQueryToRecordsReader(ctx, nullComparison); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("comparison to NULL = %v, want ErrNotSupported", err)
	}
}

func TestIntegration_ServerMaxRowsIsNotTruncation(t *testing.T) {
	endpoint, token := integrationReaderToken(t)
	transport := newRecordingTransport()
	db := integrationDBConfigured(t, endpoint, token, func(c *Config) {
		c.MaxRows = 10
		c.MaxPageSize = 10
		c.MaxRequests = 10
	}, &http.Client{Transport: transport})

	query := dal.From(dal.NewRootCollectionRef("labels", "")).NewQuery().
		OrderBy(dal.AscendingField("id")).
		Limit(6).
		SelectIntoRecord(nil)
	reader, err := db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 6 {
		t.Fatalf("records = %d, want 6; a server-capped page was treated as EOF", len(records))
	}
	for i, rec := range records {
		if got := rec.Key().ID; got != int64(i+1) {
			t.Fatalf("record %d id = %#v, want %d", i, got, i+1)
		}
	}
	requests, ranges := transport.snapshot()
	if requests != 2 {
		t.Fatalf("requests = %d (%v), want 2 even though db-max-rows is 3", requests, ranges)
	}
	if len(ranges) != 2 || !strings.HasPrefix(ranges[0], "0-2/") || !strings.HasPrefix(ranges[1], "3-5/") {
		t.Fatalf("Content-Range sequence = %v, want capped pages 0-2/* then 3-5/*", ranges)
	}

	// The same capped relation with a request budget too small must fail rather
	// than return the first page as a successful short result.
	limited := integrationDBConfigured(t, endpoint, token, func(c *Config) {
		c.MaxRows = 10
		c.MaxPageSize = 10
		c.MaxRequests = 1
	})
	recordsetQuery := dal.From(dal.NewRootCollectionRef("labels", "")).NewQuery().
		Limit(6).
		SelectIntoRecordset()
	if _, err = limited.ExecuteQueryToRecordsetReader(context.Background(), recordsetQuery); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("recordset with one request allowed = %v, want ErrBudgetExceeded", err)
	}
}

func keysOf(records []record.Record) []any {
	keys := make([]any, len(records))
	for i, rec := range records {
		keys[i] = rec.Key().ID
	}
	return keys
}
