package dalgo2postgrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

func TestGetExistsAndGetMulti(t *testing.T) {
	var mu sync.Mutex
	var requested []string
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/v1/items" {
			t.Errorf("path = %q", r.URL.Path)
		}
		query := r.URL.Query()
		mu.Lock()
		requested = append(requested, query.Get("id")+"|"+query.Get("select")+"|"+query.Get("limit"))
		mu.Unlock()
		switch query.Get("id") {
		case "eq.1":
			writeJSON(w, `[{"id":1,"title":"one"}]`)
		case "eq.abc":
			writeJSON(w, `[{"id":"abc","title":"letters"}]`)
		default:
			writeJSON(w, `[]`)
		}
	})

	first := record.NewRecordWithData(record.NewKeyWithID("items", 1), map[string]any{})
	if err := server.db.Get(context.Background(), first); err != nil {
		t.Fatalf("Get(int): %v", err)
	}
	if got := first.Key().ID; got != 1 {
		t.Fatalf("key id = %#v", got)
	}
	if got := first.Data().(map[string]any)["title"]; got != "one" {
		t.Fatalf("title = %#v", got)
	}
	if got := first.Data().(map[string]any)["id"]; got != int64(1) {
		t.Fatalf("native id was removed from Get data: %#v", first.Data())
	}

	letters := record.NewRecordWithData(record.NewKeyWithID("items", "abc"), map[string]any{})
	if err := server.db.Get(context.Background(), letters); err != nil {
		t.Fatalf("Get(string): %v", err)
	}
	if got := letters.Key().ID; got != "abc" {
		t.Fatalf("string key id = %#v", got)
	}

	missing := record.NewRecordWithData(record.NewKeyWithID("items", 99), map[string]any{})
	err := server.db.Get(context.Background(), missing)
	if !record.IsNotFound(err) {
		t.Fatalf("missing Get = %v, want not found", err)
	}
	if missing.Exists() {
		t.Fatal("missing record reports Exists()")
	}

	found, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if err != nil || !found {
		t.Fatalf("Exists(1) = %v, %v", found, err)
	}
	found, err = server.db.Exists(context.Background(), record.NewKeyWithID("items", "missing"))
	if err != nil || found {
		t.Fatalf("Exists(missing) = %v, %v", found, err)
	}

	one := record.NewRecordWithData(record.NewKeyWithID("items", 1), map[string]any{})
	two := record.NewRecordWithData(record.NewKeyWithID("items", 99), map[string]any{})
	if err = server.db.GetMulti(context.Background(), []record.Record{one, two}); err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if two.Exists() {
		t.Fatal("GetMulti missing record reports Exists()")
	}

	mu.Lock()
	gotRequests := append([]string(nil), requested...)
	mu.Unlock()
	if len(gotRequests) != 7 {
		t.Fatalf("requests = %d (%v), want 7", len(gotRequests), gotRequests)
	}
	if gotRequests[0] != "eq.1|*|1" {
		t.Fatalf("first request = %q", gotRequests[0])
	}
	if gotRequests[3] != "eq.1|id|1" {
		t.Fatalf("Exists request = %q", gotRequests[3])
	}
}

func TestGetRejectsUnsupportedKeysBeforeIO(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an HTTP request was sent for an unsupported key")
		writeJSON(w, "[]")
	})
	parent := record.NewKeyWithID("parents", "p")
	for _, key := range []*record.Key{
		record.NewKeyWithParentAndID(parent, "items", "1"),
		record.NewKeyWithFields("items", record.FieldVal{Name: "a", Value: 1}),
		record.NewKeyWithID("bad-name", "1"),
	} {
		rec := record.NewRecordWithData(key, map[string]any{})
		if err := server.db.Get(context.Background(), rec); !errors.Is(err, dal.ErrNotSupported) {
			t.Fatalf("Get(%v) = %v, want ErrNotSupported", key, err)
		}
	}
	if got := server.hits.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

type typedItem struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func TestTypedRecordTemplate(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":7,"title":"seven"}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
		SelectIntoRecord(func() record.Record {
			return record.NewRecordWithIncompleteKey("items", reflect.Int64, &typedItem{})
		})
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if got := records[0].Key().ID; got != int64(7) {
		t.Fatalf("key id = %#v, want native int64 7", got)
	}
	item, ok := records[0].Data().(*typedItem)
	if !ok {
		t.Fatalf("data type = %T", records[0].Data())
	}
	if item.ID != 7 || item.Title != "seven" {
		t.Fatalf("item = %#v", item)
	}
}

func TestNumericValuesAreNotCoercedThroughFloat64(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{
			"id":9007199254740993,
			"big":123456789012345678901234567890,
			"decimal":1.25,
			"exponent":1e3,
			"nested":{"decimal":2.5},
			"array":[9007199254740993]
		}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 1 {
		t.Fatalf("records = %d", len(records))
	}
	if got := records[0].Key().ID; got != int64(9007199254740993) {
		t.Fatalf("id = %#v (%T)", got, got)
	}
	data := records[0].Data().(map[string]any)
	assertNumber(t, data["big"], "123456789012345678901234567890")
	assertNumber(t, data["decimal"], "1.25")
	assertNumber(t, data["exponent"], "1e3")
	nested := data["nested"].(map[string]any)
	assertNumber(t, nested["decimal"], "2.5")
	items := data["array"].([]any)
	if items[0] != int64(9007199254740993) {
		t.Fatalf("array[0] = %#v", items[0])
	}
}

func assertNumber(t *testing.T, value any, want string) {
	t.Helper()
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("value = %#v (%T), want json.Number", value, value)
	}
	if number.String() != want {
		t.Fatalf("number = %q, want %q", number.String(), want)
	}
}

func TestGetExistsAndGetMultiRejectInvalidInputs(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "eq.1" {
			writeJSON(w, `[{"id":2,"title":"wrong"}]`)
			return
		}
		writeJSON(w, `[]`)
	})
	if err := server.db.Get(context.Background(), nil); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("Get(nil) = %v, want ErrNotSupported", err)
	}
	if err := server.db.GetMulti(context.Background(), []record.Record{nil}); !errors.Is(err, dal.ErrNotSupported) {
		t.Fatalf("GetMulti(nil) = %v, want ErrNotSupported", err)
	}
	_, err := server.db.Exists(context.Background(), record.NewKeyWithID("items", 1))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("Exists with a mismatched id = %v, want ErrInvalidResponse", err)
	}
}

func TestGetMultiSharesTheResponseByteBudget(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		switch id {
		case "eq.1":
			writeJSON(w, `[{"id":1,"title":"one"}]`)
		case "eq.2":
			writeJSON(w, `[{"id":2,"title":"two"}]`)
		default:
			writeJSON(w, `[]`)
		}
	}, func(c *Config) { c.MaxResponseBytes = 40 })
	one := record.NewRecordWithData(record.NewKeyWithID("items", int64(1)), map[string]any{})
	two := record.NewRecordWithData(record.NewKeyWithID("items", int64(2)), map[string]any{})
	err := server.db.GetMulti(context.Background(), []record.Record{one, two})
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("GetMulti = %v, want ErrResponseTooLarge", err)
	}
	if one.Data().(map[string]any)["title"] != "one" {
		t.Fatal("the first record was not filled")
	}
}

func TestReadersEOFIsStable(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":2},{"id":1}]`)
	})
	ctx := context.Background()

	recordsQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(ctx, recordsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(consumeRecords(t, reader)); got != 2 {
		t.Fatalf("records = %d, want 2", got)
	}
	for i := 0; i < 2; i++ {
		if _, err = reader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
			t.Fatalf("records Next after EOF = %v, want ErrNoMoreRecords (call %d)", err, i+1)
		}
	}
	_ = reader.Close()

	recordsetQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecordset()
	rsReader, err := server.db.ExecuteQueryToRecordsetReader(ctx, recordsetQuery)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err = rsReader.Next(); err != nil {
			t.Fatalf("recordset Next %d = %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, err = rsReader.Next(); !errors.Is(err, dal.ErrNoMoreRecords) {
			t.Fatalf("recordset Next after EOF = %v, want ErrNoMoreRecords (call %d)", err, i+1)
		}
	}
	_ = rsReader.Close()
}

func TestRecordsetValuesKeepNativeTypes(t *testing.T) {
	server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, `[{"id":1,"qty":10,"price":12.50,"active":true,"title":"one"}]`)
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecordset()
	reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	row, rs, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	value, err := row.GetValueByName("qty", rs)
	if err != nil || value != int64(10) {
		t.Fatalf("qty = %#v, %v", value, err)
	}
	value, err = row.GetValueByName("price", rs)
	if err != nil {
		t.Fatal(err)
	}
	assertNumber(t, value, "12.50")
	value, err = row.GetValueByName("active", rs)
	if err != nil || value != true {
		t.Fatalf("active = %#v, %v", value, err)
	}
}
