package dalgo2postgrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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

func TestRecordsReaderRechecksTerminalStateAfterBuildingRows(t *testing.T) {
	t.Run("IntoRecord callback cancels the context", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":2,"title":"two"},{"id":1,"title":"one"}]`))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).
			SelectIntoRecord(func() record.Record {
				cancel()
				return record.NewRecordWithData(record.NewKeyWithID("items", int64(0)), map[string]any{})
			})
		reader, err := server.db.ExecuteQueryToRecordsReader(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			rec, nextErr := reader.Next()
			if rec != nil {
				t.Fatalf("Next returned a record after the callback aborted the read: %#v", rec)
			}
			if !errors.Is(nextErr, context.Canceled) {
				t.Fatalf("Next %d = %v, want context.Canceled", i+1, nextErr)
			}
		}
		if got := server.hits.Load(); got != 1 {
			t.Fatalf("requests = %d, want 1; a row built before cancellation caused another request", got)
		}
		_ = reader.Close()
	})

	t.Run("IntoRecord callback closes the database", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":2,"title":"two"},{"id":1,"title":"one"}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).
			SelectIntoRecord(func() record.Record {
				if err := server.db.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
				return record.NewRecordWithData(record.NewKeyWithID("items", int64(0)), map[string]any{})
			})
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			rec, nextErr := reader.Next()
			if rec != nil {
				t.Fatalf("Next returned a record after the callback closed the database: %#v", rec)
			}
			if !errors.Is(nextErr, ErrClosed) {
				t.Fatalf("Next %d = %v, want ErrClosed", i+1, nextErr)
			}
		}
		if got := server.hits.Load(); got != 1 {
			t.Fatalf("requests = %d, want 1; a row built before close caused another request", got)
		}
		_ = reader.Close()
	})
}

func TestRecordsProjectionHonorsDeclaredOutputs(t *testing.T) {
	titleOnly := func() dal.Query {
		return dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")})
	}
	aliasAndHiddenID := func() dal.Query {
		return dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(
				dal.Column{Expression: dal.Field("title")},
				dal.Column{Alias: "q", Expression: dal.Field("qty")},
			)
	}
	keysOnly := func() dal.Query {
		return dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectKeysOnly(reflect.Int64)
	}
	wildcard := func() dal.Query {
		return dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectIntoRecord(nil)
	}
	for _, test := range []struct {
		name        string
		query       dal.Query
		selectValue string
		body        string
		wantKey     any
		wantData    map[string]any
	}{
		{
			name:        "title only keeps neither hidden id nor unexpected field",
			query:       titleOnly(),
			selectValue: "title,id",
			body:        `[{"id":7,"title":"seven","unexpected":"leak"}]`,
			wantKey:     int64(7),
			wantData:    map[string]any{"title": "seven"},
		},
		{
			name:        "explicit null stays a declared value",
			query:       titleOnly(),
			selectValue: "title,id",
			body:        `[{"id":7,"title":null}]`,
			wantKey:     int64(7),
			wantData:    map[string]any{"title": nil},
		},
		{
			name:        "alias and hidden id keep only declared outputs",
			query:       aliasAndHiddenID(),
			selectValue: "title,q:qty,id",
			body:        `[{"id":7,"title":"seven","q":5,"unexpected":"leak"}]`,
			wantKey:     int64(7),
			wantData:    map[string]any{"title": "seven", "q": int64(5)},
		},
		{
			name:        "id only keeps only the id",
			query:       keysOnly(),
			selectValue: "id",
			body:        `[{"id":7,"title":"seven","unexpected":"leak"}]`,
			wantKey:     int64(7),
			wantData:    map[string]any{"id": int64(7)},
		},
		{
			name:        "wildcard keeps every response field",
			query:       wildcard(),
			selectValue: "*",
			body:        `[{"id":7,"title":"seven","extra":"kept"}]`,
			wantKey:     int64(7),
			wantData:    map[string]any{"id": int64(7), "title": "seven", "extra": "kept"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("select"); got != test.selectValue {
					t.Errorf("select = %q, want %q", got, test.selectValue)
				}
				writeJSON(w, test.body)
			})
			reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			records := consumeRecords(t, reader)
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
			if got := records[0].Key().ID; !reflect.DeepEqual(got, test.wantKey) {
				t.Fatalf("key id = %#v (%T), want %#v", got, got, test.wantKey)
			}
			data, ok := records[0].Data().(map[string]any)
			if !ok {
				t.Fatalf("data type = %T", records[0].Data())
			}
			if !reflect.DeepEqual(data, test.wantData) {
				t.Fatalf("data = %#v, want %#v", data, test.wantData)
			}
		})
	}
}

func TestRecordsProjectionRejectsMissingDeclaredColumn(t *testing.T) {
	server := newTestDB(t, jsonRows(`[{"id":7,"unexpected":"leak"}]`))
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
		SelectColumns(dal.Column{Expression: dal.Field("title")})
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec, nextErr := reader.Next()
		if rec != nil {
			t.Fatalf("Next returned a record for a response without the declared title: %#v", rec)
		}
		if !errors.Is(nextErr, ErrInvalidResponse) {
			t.Fatalf("Next %d = %v, want ErrInvalidResponse", i+1, nextErr)
		}
	}
}

func TestRecordsetProjectionMatchesRecordsContract(t *testing.T) {
	t.Run("missing declared column is a protocol error", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":7,"unexpected":"leak"}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")})
		reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
		if reader != nil {
			t.Fatal("a recordset reader was returned for a response without the declared title")
		}
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("error = %v, want ErrInvalidResponse", err)
		}
		if got := server.hits.Load(); got != 1 {
			t.Fatalf("requests = %d, want 1", got)
		}
	})

	t.Run("explicit null is a value", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":7,"title":null}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Expression: dal.Field("title")})
		reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		rs := reader.Recordset()
		columns := rs.Columns()
		if len(columns) != 1 || columns[0].Name() != "title" {
			t.Fatalf("columns = %#v, want title", columns)
		}
		row, _, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		value, err := row.GetValueByName("title", rs)
		if err != nil || value != nil {
			t.Fatalf("title = %#v, %v, want an explicit nil without an error", value, err)
		}
	})

	t.Run("alias and hidden id", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":7,"q":10,"unexpected":"leak"}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectColumns(dal.Column{Alias: "q", Expression: dal.Field("qty")})
		reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		rs := reader.Recordset()
		columns := rs.Columns()
		if len(columns) != 1 || columns[0].Name() != "q" {
			t.Fatalf("columns = %#v, want q only", columns)
		}
		row, _, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		value, err := row.GetValueByName("q", rs)
		if err != nil || value != int64(10) {
			t.Fatalf("q = %#v, %v, want int64(10)", value, err)
		}
	})

	t.Run("id only", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":7,"title":"leak"}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectKeysOnly(reflect.Int64)
		reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		rs := reader.Recordset()
		columns := rs.Columns()
		if len(columns) != 1 || columns[0].Name() != "id" {
			t.Fatalf("columns = %#v, want id only", columns)
		}
		row, _, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		value, err := row.GetValueByName("id", rs)
		if err != nil || value != int64(7) {
			t.Fatalf("id = %#v, %v, want int64(7)", value, err)
		}
	})

	t.Run("wildcard keeps derived columns", func(t *testing.T) {
		server := newTestDB(t, jsonRows(`[{"id":7,"title":"seven","extra":"kept"}]`))
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).
			SelectIntoRecordset()
		reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		columns := reader.Recordset().Columns()
		names := make([]string, len(columns))
		for i, column := range columns {
			names[i] = column.Name()
		}
		if !reflect.DeepEqual(names, []string{"extra", "id", "title"}) {
			t.Fatalf("wildcard columns = %#v", names)
		}
	})
}

func TestPointReadsQuoteReservedIDColumn(t *testing.T) {
	for _, name := range []string{"select", "columns", "on_conflict", "order", "limit", "offset", "and", "or"} {
		t.Run(name, func(t *testing.T) {
			var query url.Values
			server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
				query = r.URL.Query()
				writeJSON(w, `[{"`+name+`":7,"title":"seven"}]`)
			}, func(c *Config) { c.IDColumn = name })
			ctx := context.Background()

			rec := record.NewRecordWithData(record.NewKeyWithID("items", int64(7)), map[string]any{})
			if err := server.db.Get(ctx, rec); err != nil {
				t.Fatalf("Get: %v", err)
			}
			quoted := `"` + name + `"`
			if got := query.Get(quoted); got != "eq.7" {
				t.Fatalf("Get equality on %q = %q, want eq.7", name, got)
			}
			if got := query.Get("limit"); got != "1" {
				t.Fatalf("Get limit control = %q, want 1", got)
			}
			if name == "select" {
				if got := query.Get("select"); got != "*" {
					t.Fatalf("Get select control = %q, want *", got)
				}
			}

			found, err := server.db.Exists(ctx, record.NewKeyWithID("items", int64(7)))
			if err != nil || !found {
				t.Fatalf("Exists = %v, %v", found, err)
			}
			if got := query.Get(quoted); got != "eq.7" {
				t.Fatalf("Exists equality on %q = %q, want eq.7", name, got)
			}
			if got := query.Get("limit"); got != "1" {
				t.Fatalf("Exists limit control = %q, want 1", got)
			}
			if got := query.Get("select"); got != name {
				t.Fatalf("Exists select control = %q, want %q", got, name)
			}
			if got := server.hits.Load(); got != 2 {
				t.Fatalf("requests = %d, want 2", got)
			}
		})
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
