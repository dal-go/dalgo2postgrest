# dalgo2postgrest

A bounded, read-only [DALgo](https://github.com/dal-go/dalgo) adapter for
relations exposed by [PostgREST](https://postgrest.org/). It reads native
application tables and views through PostgREST's HTTP API, including Supabase's
`/rest/v1` endpoint. It does not speak the PostgreSQL wire protocol, does not
create a DALgo storage layout, and never writes.

Tested against **PostgREST v16.5** with **PostgreSQL 17**. CI runs the adapter
against that real pair on every push and pull request.

## Security and credentials

PostgREST authenticates an HTTP request and the database authorizes it. The
adapter never holds database credentials.

- Supply a per-request `Authorization` (and, for Supabase, an `apikey`) header
  through `Config.Headers`. The provider receives the operation context, so it
  can refresh a short-lived token or read a request-scoped value.
- Use a dedicated read-only database role with only the required `SELECT`
  grants and row-level-security policies. The adapter exposes no write path,
  but the database role remains the authority for what can be read.
- `Config.Endpoint` must be HTTPS. Plain HTTP is accepted only for a loopback
  host when `Config.AllowInsecureLoopback` is true; use that for local
  development and tests, not hosted deployments.
- Userinfo, a query string and a fragment are rejected in the endpoint.
- Redirects are always refused, including when the caller supplies an
  `http.Client` whose own policy would follow them.
- Header values containing CR/LF or NUL, invalid header names, hop-by-hop
  headers and headers the adapter owns (`Accept`, `Accept-Profile`, `Prefer`,
  `Range`, `Host`, `Content-Length`, ...) are rejected before I/O.
- PostgREST error bodies can contain row values and SQL. They are never logged
  or returned. A non-2xx response becomes an `*HTTPError` carrying only the
  status; `errors.Is(err, ErrUnauthorized)` is true for 401 and 403.
- Transport, header-provider, redirect and body failures become redacted
  sentinel errors. Context cancellation and deadline errors pass through
  normally.

Every operation has one deadline that covers header retrieval, every HTTP
exchange, response-body reading and all pages.

## Install

```sh
go get github.com/dal-go/dalgo2postgrest
```

The module is MIT licensed. Releases are cut by the fleet CI from a reviewed
`main` commit; do not tag or publish a release by hand.

## Public API

Compile the examples below with `go test ./...`; the same snippets live as
`Example` functions in `example_test.go`, so a wrong import path or a shadowed
package name fails validation instead of staying in prose.

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo2postgrest"
)

func main() {
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
```

`NewDB(ctx, Config) (dal.DB, error)` validates configuration and performs no
network I/O; the first read authenticates. The returned value is a
`*dalgo2postgrest.Database`, which embeds the sealed `dal.DB`, adds
`Close() error` and implements `dbschema.SchemaReader`. `Close` releases idle
HTTP connections and makes later operations return `ErrClosed`; it is
idempotent, and callers that only hold a `dal.DB` can use
`db.(interface{ Close() error })`.

Optional capabilities are reachable through the wrapper:

```go
schema, ok := dal.As[dbschema.SchemaReader](db)
concrete, ok := dal.As[*dalgo2postgrest.Database](db)
```

### Point reads

```go
rec := record.NewRecordWithData(record.NewKeyWithID("items", int64(1)), map[string]any{})
if err := db.Get(ctx, rec); err != nil {
	if rec.IsNotFound(err) {
		// absent
	}
	panic(err)
}
found, err := db.Exists(ctx, record.NewKeyWithID("items", int64(1)))
```

`Get` keeps the caller's record key and fills its data target with the native
row, including the configured ID column. `GetMulti` is a sequential loop under
one deadline and one shared response-byte budget; a missing record is left with
a not-found error and does not stop the other reads. At most
`MaxGetManyKeys` records are accepted. An `IDColumn` named like a PostgREST
control (`limit`, `offset`, `select`, ...) is quoted in the equality predicate
instead of overwriting that control.

## Supported query surface

`ExecuteQueryToRecordsReader` and `ExecuteQueryToRecordsetReader` accept a DALgo
`dal.StructuredQuery` over one exposed relation.

| Capability | Mapping |
| --- | --- |
| Relation | One root `dal.CollectionRef`. No parent, join, collection group or named database. Names must be simple SQL identifiers (`[A-Za-z_][A-Za-z0-9_]*`, at most 63 bytes). |
| Projection | Plain field references with optional aliases. `select=alias:field,...`. A hidden `id` column is fetched for the key and removed from the row when the caller did not project it. A bare `*` is supported; a wildcard mixed with any explicit column or alias is refused in either order, as are wildcard exclusions and source-qualified wildcards. |
| Filters | `=`, `>`, `>=`, `<`, `<=`, `IN`, `NOT IN`, explicit `dal.IsNullCondition` / `dal.IsNotNullCondition` (`is.null` / `not.is.null`), and nested `AND`/`OR` groups. Values are URL parameters, never SQL text. A comparison with `nil` is refused before I/O because DALgo defines it as UNKNOWN, not as a NULL match. |
| Ordering | Plain ascending/descending fields. The configured ID column is appended as `id.asc` unless the caller already orders by it, so offset pagination has a stable tie-breaker. |
| Paging | `offset` and `limit`. A positive limit is required; pages never exceed `MaxPageSize`, `MaxRows`, `MaxRequests` or the total response-byte budget. A short page is verified with `Content-Range` and continuation rather than assumed to be EOF. |
| Records | Numeric keys and string keys are preserved. JSON integers that fit `int64` become `int64`. Decimals, exponents and integers beyond `int64` stay `json.Number`, so no digits pass through `float64`. |
| Recordsets | A materialized columnar recordset. Explicit projections keep their column order and enforce the same declared-output contract as records: a declared column missing from the response is `ErrInvalidResponse`, while an explicit JSON `null` stays `nil`. A wildcard derives sorted column names. Array/object values and decimals remain native JSON values. |
| Not queried | No `Prefer: count=exact`, no hidden count, no RPC, no resource embedding, no full-text search and no server-side aggregation. |

PostgREST reserves the query parameters `select`, `columns`, `on_conflict`,
`order`, `limit`, `offset`, `and` and `or`. A physical column with one of those
exact names remains usable: filter keys and the equality predicate of a
reserved `IDColumn` are double-quoted with PostgREST's official identifier
quoting (for example `?"limit"=eq.10`), so a constraint is never silently
consumed as a control. The same quoting is used inside nested `AND`/`OR`
expressions.

Unsupported operations wrap `dal.ErrNotSupported` and are refused **before any
HTTP request**: joins, aggregates, `GROUP BY`, `HAVING`, subqueries, cursors,
native text/SQL queries, grouping, array operators other than membership, writes
and callback transactions. PostgREST cannot provide an atomic callback
transaction across HTTP requests, so both transaction methods refuse rather than
pretend.

The core DALgo aggregation fallback removes the caller's limit and asks the
adapter for unbounded source rows. This adapter refuses every limit-zero read,
so a local aggregate fails instead of returning a partial aggregate computed
from a hidden cap. A CLI integration must supply an explicit positive default
limit for ordinary queries.

### Bounds

| Config | Default | Hard maximum | Meaning |
| --- | --- | --- | --- |
| `MaxRows` | 1000 | 10000 | Largest accepted query limit. |
| `MaxPageSize` | 200 | `MaxRows` | Rows requested per HTTP page. |
| `MaxRequests` | 20 | 100 | Pages one query may request. |
| `MaxResponseBytes` | 4 MiB | 64 MiB | Total response bytes of one operation, across all pages. |
| `MaxMetadataBytes` | 8 MiB | 64 MiB | OpenAPI document byte budget. |
| `MaxOffset` | 100000 | 10000000 | Largest accepted offset. |
| `MaxGetManyKeys` | 100 | 1000 | Keys accepted by `GetMulti`. |
| `Timeout` | 15s | 2m | One operation deadline, including auth, HTTP and body. |

A query whose page count, row count or offset cannot fit the configured budget
is refused with `ErrBudgetExceeded` before I/O. A response above the byte budget
stops the operation with `ErrResponseTooLarge`; a reader never reports a
successful partial aggregate.

Shape limits are fixed: one top-level relation, at most 16 ordering fields, 256
projection columns, 64 filter conditions nested at most 8 levels deep, 100 values
in one `IN`/`NOT IN` list, 4096 bytes per string filter value, and 32 KiB of
encoded query parameters. A query above a shape limit is refused before I/O.

### Pagination integrity

PostgREST can return fewer rows than requested even when more exist, for
example when its `db-max-rows` setting is below `MaxPageSize`. A short page is
therefore **not** treated as end of data. The adapter:

1. validates a present `Content-Range` against the requested offset and the
   returned row count, rejecting a malformed, repeated or out-of-order range;
2. continues from the next offset while rows remain and the caller's `limit`
   has not been reached;
3. finishes only on an empty page, a known total from `Content-Range`, or the
   caller's positive limit.

A missing `Content-Range` is tolerated because continuation still requires an
empty page, so a missing header cannot silently truncate a result. If the
configured `MaxRequests` is too small for the continuation pages, the read fails
with `ErrBudgetExceeded`; it never returns a successful short result. The same
budget applies when a provider returns one row per request.

## Metadata

`*Database` implements `dbschema.SchemaReader` using the permitted, bounded
PostgREST OpenAPI document at the API root. The same header provider and
`Accept-Profile` profile are used, and only one root request is made per call.

- `ListCollections` lists relations in `definitions` and one-segment paths the
  role can see. A valid OpenAPI document that exposes no relation to the role is
  an empty list; a missing, unauthorized or malformed document is an error, never
  an empty list.
- `DescribeCollection` preserves the property order, reports portable field
  types (`integer`, `number`, `boolean`, `string`, date/time formats, binary),
  derives `Nullable` from OpenAPI `required`, and reports a primary key only when
  PostgREST's `<pk/>` marker is present. Native OpenAPI formats are retained in
  `SourceDefinition.Columns[].DeclaredType`. Arrays, objects and `json/jsonb` are
  reported as `dbschema.Null` because the portable type enumeration has no JSON
  type.
- Local `$ref`s are resolved inside the same document. External `$ref`s are
  never fetched; the call fails with `ErrMetadataUnavailable`.
- `ListIndexes`, `ListConstraints` and `ListReferrers` return
  `*dbschema.NotSupportedError` (wrapping `dal.ErrNotSupported`): PostgREST
  OpenAPI does not expose that metadata, and the adapter does not invent it.
- `ErrMetadataUnauthorized` and `*MetadataError` distinguish a denied root
  document from an unusable one. Error messages never include the body.

## Known limitations

- Offset pagination is stable within one request but not a database snapshot.
  Concurrent writes can shift rows between pages. Use a stable filter on the ID
  or another unique key when that matters.
- The configured ID column must be unique and exposed. Composite primary keys,
  non-unique IDs and relations without the configured column are not supported
  as record identities.
- Relation, field and alias names must be simple ASCII SQL identifiers. Metadata
  can still list a relation whose name a query would refuse.
- A column whose name is a PostgREST control (`limit`, `offset`, `select`,
  `order`, `or`, ...) is targeted with quoted filter keys. This is verified
  against PostgREST v16.5; a server or proxy that rewrites or rejects quoted
  query keys fails the read instead of silently dropping the predicate.
- Explicit projections are exact for both records and recordsets: every declared
  output column must be present in the response. An explicit JSON `null` is a
  present value and stays `nil`; a missing column fails with
  `ErrInvalidResponse`.
- PostgREST's own `db-max-rows` may cap a page below the requested `MaxPageSize`.
  The adapter continues with Content-Range-aware paging and needs enough
  `MaxRequests` to cover the resulting pages; if the budget is too small the
  read fails with `ErrBudgetExceeded` rather than returning a short success.
  It does not send `count=exact`.
- A wildcard recordset read derives its columns from the rows it receives, so an
  empty result has no columns. Use an explicit projection when an empty result
  must still carry its column shape.
- Timestamps are returned as the strings PostgREST writes; metadata reports
  their portable type as time.
- The adapter supports PostgREST tables/views only. RPC functions, embedding,
  full-text search, aggregates, grouping and writes are deliberately absent.
- Hosted Supabase acceptance is a manual gate (below). The checked-in
  integration suite proves the owned PostgREST v16.5 fixture, not a hosted
  project, its JWT verifier, RLS policy or network path.

## Tests

Unit and protocol tests use `httptest` and need no external service:

```sh
go test ./...
go test -race -count=1 ./...
go vet ./...
```

The opt-in real-service suite starts a disposable PostgreSQL 17 + PostgREST
v16.5 Compose project with random loopback ports and a unique Compose project
name, seeds application rows through PostgreSQL's init hook, exercises rows,
filters, ordering, paging, recordsets, metadata and auth failures, then removes
its containers and volume. The fixture sets `db-max-rows=3` (below the adapter's
default `MaxPageSize`) and seeds distinct empty-string, NULL and non-null values
in `app.labels`, so the real server proves both pagination continuation and
NULL/empty-string separation. It also seeds `app.reserved_words` and
`app.reserved_ids`, whose column names are PostgREST controls (`limit`,
`offset`, `select`, `order`, `or`, ...), to prove the quoted filters and the
reserved `IDColumn` point reads against the real service.

```sh
./scripts/integration.sh
```

The script also verifies that the served `Server` header is
`postgrest/16.5`, that exactly four `items` and six `labels` rows were seeded
externally, and that the `app_reader` role has `SELECT` but not `INSERT`. It
fails if the integration tests skip, and it fails if teardown leaves a
container.

Fixture files: `testdata/docker-compose.yml` and `testdata/seed.sql`.
Environment variables for a direct run: `POSTGREST_TEST_URL`,
`POSTGREST_TEST_JWT_SECRET`, `POSTGREST_TEST_READER_ROLE`,
`POSTGREST_TEST_NOACCESS_ROLE`; optional `POSTGREST_TEST_READER_TOKEN` and
`POSTGREST_TEST_API_KEY` for a hosted Supabase run.

## Manual hosted Supabase acceptance gate

Local PostgREST proves the protocol and bounded adapter behaviour. A hosted
Supabase project remains a separate gate because its JWT verifier, CORS,
network, grants and RLS policies are deployment-specific. To run it:

1. Use a project and schema where creating a dedicated `items` table is allowed.
   Create the table and four synthetic rows from `testdata/seed.sql` (drop the
   role creation and local `app` schema parts, or use a dedicated schema and set
   `Config.Schema`).
2. Grant `SELECT` on the table to a scoped read-only role (or use Supabase RLS
   with the `anon`/authenticated role), and deny `INSERT`, `UPDATE` and
   `DELETE`.
3. Obtain a short-lived access token for that identity. Do not use the
   service-role key and do not commit any secret.
4. Run the hosted read journey with the Supabase anon key and token supplied as
   headers:

   ```sh
   POSTGREST_TEST_URL="https://<ref>.supabase.co/rest/v1" \
   POSTGREST_TEST_READER_TOKEN="<scoped access token>" \
   POSTGREST_TEST_API_KEY="<anon key>" \
   go test -run '^TestIntegration_ReadsSeededRelation$' -v -count=1 ./...
   ```

5. Also exercise a deliberately invalid token, a page after page, metadata and
   `Close` through the real application, and confirm that error text contains
   neither the token nor the URL.
6. Record the Supabase project/region, PostgREST version reported by the
   endpoint, role, RLS policy and date. Until that record exists, treat hosted
   Supabase support as unverified in this repository.

## License

MIT. See [LICENSE](LICENSE).
