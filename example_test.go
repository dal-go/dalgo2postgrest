package dalgo2postgrest_test

// The examples below are the README snippets kept as Go examples. They have no
// Output comment, so `go test` compiles and type-checks them without executing
// them against a real endpoint. This prevents the published import path and the
// point-read variable names from drifting back into prose-only mistakes.

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2postgrest"
	"github.com/dal-go/record"
)

func ExampleNewDB() {
	ctx := context.Background()
	db, err := dalgo2postgrest.NewDB(ctx, dalgo2postgrest.Config{
		Endpoint: "https://<project>.supabase.co/rest/v1",
		// Optional: selects a PostgREST schema profile with Accept-Profile.
		Schema: "",
		// Optional: defaults to "id".
		IDColumn: "id",
		Headers: func(ctx context.Context) (http.Header, error) {
			return http.Header{
				"Authorization": {"Bearer " + os.Getenv("SUPABASE_ACCESS_TOKEN")},
				"apikey":        {os.Getenv("SUPABASE_ANON_KEY")},
			}, nil
		},
		// Optional: use MaxRows, MaxPageSize, MaxRequests,
		// MaxResponseBytes, MaxMetadataBytes, MaxOffset, MaxGetManyKeys and
		// Timeout to narrow the defaults.
		MaxRows: 100,
	})
	if err != nil {
		panic(err)
	}
	defer db.(*dalgo2postgrest.Database).Close()

	query := dal.From(dal.NewRootCollectionRef("items", "")).NewQuery().
		WhereField("qty", dal.GreaterOrEqual, 10).
		OrderBy(dal.DescendingField("qty")).
		Limit(50). // a positive limit is required
		SelectColumns(dal.Column{Expression: dal.Field("title")})

	reader, err := db.ExecuteQueryToRecordsReader(ctx, query)
	if err != nil {
		panic(err)
	}
	defer func() { _ = reader.Close() }()
	for {
		rec, err := reader.Next()
		if err == dal.ErrNoMoreRecords {
			break
		}
		if err != nil {
			panic(err)
		}
		fmt.Printf("%v %v\n", rec.Key().ID, rec.Data())
	}
}

func ExampleDatabase_Get() {
	ctx := context.Background()
	var db dal.DB

	rec := record.NewRecordWithData(record.NewKeyWithID("items", int64(1)), map[string]any{})
	if err := db.Get(ctx, rec); err != nil {
		if record.IsNotFound(err) {
			// absent
			return
		}
		panic(err)
	}
	found, err := db.Exists(ctx, record.NewKeyWithID("items", int64(1)))
	if err != nil || !found {
		panic(fmt.Errorf("exists: %v, found=%v", err, found))
	}
}
