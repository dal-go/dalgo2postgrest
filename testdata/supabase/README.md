# Supabase-style REST fixture (stage 2)

A disposable, owned three-service fixture for the `dalgo2postgrest` adapter:

| service | pinned image | exposure |
| --- | --- | --- |
| `postgres` | `postgres:18@sha256:979bccf…` | no host port |
| `postgrest` | `postgrest/postgrest:v16.5@sha256:1347d75…` | no host port |
| `kong` | `kong/kong:3.9.3@sha256:9a2ae66…` | one random `127.0.0.1` proxy port; admin off |

Kong is the only public listener. Its declarative configuration
(`kong.yml`) refuses the exact PostgREST OpenAPI root to the synthetic public
fixture key through real `key-auth` + `acl` plugins, while the
`/rest/v1/<relation>` route forwards the caller's `Authorization` bearer to
PostgREST.

`seed.sql` inserts rows only through PostgreSQL's init hook. The
`authenticated` role has `SELECT` only on owner-scoped rows; the file contains
no account, payment, production or service-role object.

## Running the acceptance

Stage 1 checks are hermetic and run with the normal Go suite:

```sh
go test -count=1 -run '^TestSupabaseFixture' ./...
./scripts/test-supabase-lifecycle.sh
shellcheck scripts/*.sh scripts/lib/*.sh
actionlint .github/workflows/ci.yml
```

Stage 2 starts containers and is opt-in:

```sh
./scripts/check-supabase-rest.sh
```

The script verifies the externally seeded rows and raw HTTP/auth/RLS premises,
runs `TestSupabaseIntegration*` against the owned gateway, runs the separate
public-module consumer pinned to the published SDK, and (when the installed
CLI is present) invokes that installed `datatug` binary read-only. A run where
`SUPABASE_SKIP_INSTALLED_CLI=1` still proves the fixture and the public module,
but explicitly does **not** claim installed-release acceptance.
