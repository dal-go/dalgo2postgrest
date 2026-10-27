// Package dalgo2postgrest provides a bounded, read-only DALgo adapter for
// relations exposed by PostgREST, including Supabase's REST endpoint.
//
// It reads native application tables and views through PostgREST's HTTP API. It
// does not speak the PostgreSQL wire protocol, does not create a DALgo storage
// layout, and never writes.
//
// NewDB validates the endpoint, limits and header provider and returns a
// dal.DB. The optional dbschema.SchemaReader capability is implemented by the
// concrete *Database value and is reachable with dal.As. Call Close when the
// handle is no longer needed.
package dalgo2postgrest
