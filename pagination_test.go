package dalgo2postgrest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/dal-go/dalgo/dal"
)

// cappedPages serves at most cap rows per request, like PostgREST with
// db-max-rows smaller than the requested page. With rangeHeader it also sends a
// truthful Content-Range for the served slice.
func cappedPages(dataset []string, cap int, rangeHeader bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if offset > len(dataset) {
			offset = len(dataset)
		}
		if limit > cap {
			limit = cap
		}
		end := offset + limit
		if end > len(dataset) {
			end = len(dataset)
		}
		rows := dataset[offset:end]
		contentRange := ""
		if rangeHeader {
			if len(rows) == 0 {
				contentRange = "*/0"
			} else {
				contentRange = fmt.Sprintf("%d-%d/*", offset, end-1)
			}
		}
		writePage(w, "["+strings.Join(rows, ",")+"]", contentRange)
	}
}

func TestServerCappedPageIsContinued(t *testing.T) {
	dataset := []string{
		`{"id":1}`, `{"id":2}`, `{"id":3}`, `{"id":4}`, `{"id":5}`,
	}
	var offsets []string
	server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset")+"/"+r.URL.Query().Get("limit"))
		cappedPages(dataset, 2, true)(w, r)
	}, func(c *Config) {
		c.MaxRows = 10
		c.MaxPageSize = 3
		c.MaxRequests = 10
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecord(nil)
	reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	records := consumeRecords(t, reader)
	if len(records) != 5 {
		t.Fatalf("records = %d, want all 5 rows across capped pages", len(records))
	}
	for i, want := range []int64{1, 2, 3, 4, 5} {
		if got := records[i].Key().ID; got != want {
			t.Fatalf("record %d id = %#v, want %d", i, got, want)
		}
	}
	if got := strings.Join(offsets, ","); got != "0/3,2/3,4/1" {
		t.Fatalf("page offsets = %s", got)
	}
	if got := server.hits.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestOneRowPerRequestStaysWithinBudget(t *testing.T) {
	dataset := []string{
		`{"id":1}`, `{"id":2}`, `{"id":3}`, `{"id":4}`, `{"id":5}`,
		`{"id":6}`, `{"id":7}`, `{"id":8}`, `{"id":9}`, `{"id":10}`,
	}
	newServer := func(mutate ...func(*Config)) *testServer {
		return newTestDB(t, cappedPages(dataset, 1, true), mutate...)
	}
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecord(nil)

	t.Run("complete within budget", func(t *testing.T) {
		server := newServer(func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 5
			c.MaxRequests = 10
		})
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		records := consumeRecords(t, reader)
		if len(records) != 5 {
			t.Fatalf("records = %d, want 5", len(records))
		}
		if got := server.hits.Load(); got != 5 {
			t.Fatalf("requests = %d, want 5", got)
		}
	})

	t.Run("request budget is enforced", func(t *testing.T) {
		server := newServer(func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 5
			c.MaxRequests = 2
		})
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		var read int
		for {
			_, err = reader.Next()
			if err == nil {
				read++
				continue
			}
			if errors.Is(err, dal.ErrNoMoreRecords) {
				t.Fatal("the reader reported clean EOF instead of a budget failure")
			}
			if !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("Next = %v, want ErrBudgetExceeded", err)
			}
			break
		}
		if read != 2 {
			t.Fatalf("read = %d rows before the budget error, want 2", read)
		}
		if got := server.hits.Load(); got != 2 {
			t.Fatalf("requests = %d, want exactly the 2 allowed", got)
		}
	})

	t.Run("recordset refuses instead of truncating", func(t *testing.T) {
		server := newServer(func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 5
			c.MaxRequests = 2
		})
		recordsetQuery := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecordset()
		if _, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), recordsetQuery); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("recordset error = %v, want ErrBudgetExceeded", err)
		}
	})
}

func TestRecordsetContinuesCappedPages(t *testing.T) {
	dataset := []string{
		`{"id":1,"title":"one"}`, `{"id":2,"title":"two"}`, `{"id":3,"title":"three"}`,
		`{"id":4,"title":"four"}`, `{"id":5,"title":"five"}`,
	}
	server := newTestDB(t, cappedPages(dataset, 2, true), func(c *Config) {
		c.MaxRows = 10
		c.MaxPageSize = 3
		c.MaxRequests = 10
	})
	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecordset()
	reader, err := server.db.ExecuteQueryToRecordsetReader(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if got := reader.Recordset().RowsCount(); got != 5 {
		t.Fatalf("recordset rows = %d, want 5", got)
	}
}

func TestContentRangeProtocol(t *testing.T) {
	t.Run("missing header is continued to a verified empty page", func(t *testing.T) {
		dataset := []string{`{"id":4}`, `{"id":3}`, `{"id":2}`, `{"id":1}`}
		var hits int
		server := newTestDB(t, func(w http.ResponseWriter, r *http.Request) {
			hits++
			cappedPages(dataset, 2, false)(w, r)
		}, func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 2
			c.MaxRequests = 10
		})
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(4).SelectIntoRecord(nil)
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		records := consumeRecords(t, reader)
		if len(records) != 4 {
			t.Fatalf("records = %d, want 4", len(records))
		}
		if hits != 2 {
			t.Fatalf("requests = %d, want 2", hits)
		}
	})

	t.Run("known total finishes without an extra page", func(t *testing.T) {
		var hits int
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			hits++
			writePage(w, `[{"id":1},{"id":2}]`, "0-1/2")
		}, func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 2
		})
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(5).SelectIntoRecord(nil)
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if records := consumeRecords(t, reader); len(records) != 2 {
			t.Fatalf("records = %d, want 2", len(records))
		}
		if hits != 1 {
			t.Fatalf("requests = %d, want 1", hits)
		}
	})

	t.Run("empty page with a zero total", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			writePage(w, "[]", "*/0")
		})
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(2).SelectIntoRecord(nil)
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if records := consumeRecords(t, reader); len(records) != 0 {
			t.Fatalf("records = %d, want 0", len(records))
		}
	})

	for _, test := range []struct {
		name   string
		body   string
		header string
	}{
		{"not a range", `[{"id":1}]`, "not-a-range"},
		{"wrong start", `[{"id":1}]`, "1-1/*"},
		{"count mismatch", `[{"id":1},{"id":2}]`, "0-2/*"},
		{"end at total", `[{"id":1},{"id":2}]`, "0-1/1"},
		{"negative start", `[{"id":1}]`, "-1-0/*"},
		{"unsatisfied with rows", `[{"id":1}]`, "*/*"},
		{"empty range with rows", `[{"id":1}]`, "0-0/0"},
		{"empty page claims rows", `[]`, "*/5"},
		{"empty page with concrete range", `[]`, "0-0/*"},
		{"missing total", `[{"id":1}]`, "0-0/"},
	} {
		t.Run("invalid "+test.name, func(t *testing.T) {
			server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
				writePage(w, test.body, test.header)
			})
			query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(1).SelectIntoRecord(nil)
			reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = reader.Next(); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Next = %v, want ErrInvalidResponse", err)
			}
			if strings.Contains(err.Error(), test.header) {
				t.Fatalf("error leaked the header: %v", err)
			}
		})
	}

	t.Run("repeated start is rejected", func(t *testing.T) {
		server := newTestDB(t, func(w http.ResponseWriter, _ *http.Request) {
			writePage(w, `[{"id":1}]`, "0-0/*")
		}, func(c *Config) {
			c.MaxRows = 10
			c.MaxPageSize = 1
			c.MaxRequests = 5
		})
		query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().Limit(3).SelectIntoRecord(nil)
		reader, err := server.db.ExecuteQueryToRecordsReader(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = reader.Next(); err != nil {
			t.Fatalf("first Next = %v", err)
		}
		if _, err = reader.Next(); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("second Next = %v, want ErrInvalidResponse for a repeated page", err)
		}
	})
}
