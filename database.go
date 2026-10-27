package dalgo2postgrest

import (
	"context"
	"fmt"
	"net/url"
	"reflect"
	"sync/atomic"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
)

// Database is the concrete PostgREST handle returned by NewDB. It embeds the
// sealed dal.DB so the framework read path is preserved, and adds Close and the
// optional dbschema.SchemaReader capability. Use dal.As[dbschema.SchemaReader]
// rather than asserting an interface on the DB value directly.
type Database struct {
	dal.DB
	cfg    config
	closed atomic.Bool
}

// NewDB validates cfg and returns a read-only dal.DB backed by the PostgREST
// endpoint. It performs no network I/O: the first read authenticates. A
// canceled ctx is returned as its error. Call Close on the concrete *Database
// (or through interface{ Close() error }) when finished.
func NewDB(ctx context.Context, cfg Config) (dal.DB, error) {
	resolved, err := validateConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	database := &Database{cfg: resolved}
	database.DB = dal.NewDB(&backend{db: database})
	return database, nil
}

// Close releases idle HTTP connections and makes later operations fail with
// ErrClosed. It is idempotent.
func (d *Database) Close() error {
	if d.closed.CompareAndSwap(false, true) {
		d.cfg.client.CloseIdleConnections()
	}
	return nil
}

// Unwrap returns the sealed DALgo DB the concrete handle embeds.
func (d *Database) Unwrap() dal.DB {
	return d.DB
}

func (d *Database) closedErr() error {
	if d.closed.Load() {
		return ErrClosed
	}
	return nil
}

func (d *Database) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, d.cfg.timeout)
}

func (d *Database) relationURL(collection string, params url.Values) *url.URL {
	target := *d.cfg.base
	target.Path = d.cfg.base.Path + "/" + collection
	target.RawPath = ""
	target.RawQuery = params.Encode()
	return &target
}

func (d *Database) metadataURL() *url.URL {
	target := *d.cfg.base
	target.RawQuery = ""
	return &target
}

type backend struct {
	dal.ConcurrencyAvailable
	db *Database
}

var _ dal.Backend = (*backend)(nil)

func (b *backend) ID() string {
	id := "postgrest:" + b.db.cfg.base.Host + b.db.cfg.base.Path
	if b.db.cfg.schema != "" {
		id = "postgrest:" + b.db.cfg.schema + "@" + b.db.cfg.base.Host + b.db.cfg.base.Path
	}
	return id
}

func (b *backend) Adapter() dal.Adapter {
	return dal.NewAdapter("dalgo2postgrest", Version)
}

func (b *backend) Schema() dal.Schema {
	return nil
}

func (b *backend) RunReadonlyTransaction(context.Context, dal.ROTxWorker, ...dal.TransactionOption) error {
	if err := b.db.closedErr(); err != nil {
		return err
	}
	return notSupported("PostgREST runs every HTTP request in its own database transaction; a callback read-only transaction cannot be atomic")
}

func (b *backend) RunReadwriteTransaction(context.Context, dal.RWTxWorker, ...dal.TransactionOption) error {
	if err := b.db.closedErr(); err != nil {
		return err
	}
	return notSupported("the PostgREST adapter is read-only and does not expose callback transactions")
}

func (b *backend) Get(ctx context.Context, rec record.Record) error {
	if rec == nil {
		return notSupported("a record is required")
	}
	if err := b.db.closedErr(); err != nil {
		rec.SetError(err)
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			rec.SetError(err)
			return err
		}
	}
	opCtx, cancel := b.db.operationContext(ctx)
	defer cancel()
	budget := b.db.cfg.maxResponseBytes
	return b.get(opCtx, rec, &budget)
}

// get is Get on a context that already carries the operation deadline. budget
// is the remaining response-byte allowance of the operation.
func (b *backend) get(ctx context.Context, rec record.Record, budget *int64) error {
	if rec == nil {
		return notSupported("a record is required")
	}
	key := rec.Key()
	literal, err := b.positionalKey(key)
	if err != nil {
		rec.SetError(err)
		return err
	}
	row, found, err := b.fetchOne(ctx, key.Collection(), literal, budget)
	if err != nil {
		rec.SetError(err)
		return err
	}
	if !found {
		err = dal.NewErrNotFoundByKey(key, nil)
		rec.SetError(err)
		return err
	}
	rec.SetError(nil)
	if err = record.MapToData(rec.Data(), row); err != nil {
		rec.SetError(err)
		return err
	}
	return nil
}

func (b *backend) Exists(ctx context.Context, key *record.Key) (bool, error) {
	if err := b.db.closedErr(); err != nil {
		return false, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	literal, err := b.positionalKey(key)
	if err != nil {
		return false, err
	}
	opCtx, cancel := b.db.operationContext(ctx)
	defer cancel()
	params := url.Values{}
	params.Set("select", b.db.cfg.idColumn)
	params.Set("limit", "1")
	params.Set(b.db.cfg.idColumn, "eq."+literal)
	body, err := b.db.getBytes(opCtx, "exists", b.db.relationURL(key.Collection(), params), "application/json", b.db.cfg.maxResponseBytes)
	if err != nil {
		return false, err
	}
	rows, err := decodeRows(body, 1)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	id, ok := rows[0][b.db.cfg.idColumn]
	if !ok || id == nil {
		return false, fmt.Errorf("%w: response row is missing the configured id column", ErrInvalidResponse)
	}
	rowLiteral, err := scalarLiteral(id, false)
	if err != nil || rowLiteral != literal {
		return false, fmt.Errorf("%w: response row does not match the requested key", ErrInvalidResponse)
	}
	return true, nil
}

func (b *backend) GetMulti(ctx context.Context, records []record.Record) error {
	if err := b.db.closedErr(); err != nil {
		return err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if len(records) > b.db.cfg.maxGetManyKeys {
		return fmt.Errorf("%w: GetMulti accepts at most %d keys", ErrBudgetExceeded, b.db.cfg.maxGetManyKeys)
	}
	opCtx, cancel := b.db.operationContext(ctx)
	defer cancel()
	budget := b.db.cfg.maxResponseBytes
	for _, rec := range records {
		if rec == nil {
			return notSupported("a record is required")
		}
		err := b.get(opCtx, rec, &budget)
		if err == nil || record.IsNotFound(err) {
			continue
		}
		return err
	}
	return nil
}

// positionalKey validates a record key for a PostgREST relation and returns the
// PostgREST literal used in an equality filter. It performs no I/O.
func (b *backend) positionalKey(key *record.Key) (string, error) {
	if key == nil {
		return "", notSupported("a record key is required")
	}
	if key.Parent() != nil {
		return "", notSupported("nested DALgo keys; PostgREST relations are one level deep")
	}
	if !validIdentifier(key.Collection()) {
		return "", notSupported("relation names must be simple SQL identifiers")
	}
	if key.ID == nil {
		return "", notSupported("PostgREST record keys need a present id value")
	}
	if !comparableValue(key.ID) {
		return "", notSupported("PostgREST record ids must be comparable scalar values")
	}
	literal, err := scalarLiteral(key.ID, false)
	if err != nil {
		return "", err
	}
	return literal, nil
}

func (b *backend) fetchOne(ctx context.Context, collection, literal string, budget *int64) (map[string]any, bool, error) {
	limit := b.db.cfg.maxResponseBytes
	if budget != nil {
		limit = *budget
	}
	if limit <= 0 {
		return nil, false, ErrResponseTooLarge
	}
	params := url.Values{}
	params.Set("select", "*")
	params.Set("limit", "1")
	params.Set(b.db.cfg.idColumn, "eq."+literal)
	body, err := b.db.getBytes(ctx, "get", b.db.relationURL(collection, params), "application/json", limit)
	if err != nil {
		return nil, false, err
	}
	if budget != nil {
		*budget -= int64(len(body))
	}
	rows, err := decodeRows(body, 1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	row := rows[0]
	id, ok := row[b.db.cfg.idColumn]
	if !ok || id == nil {
		return nil, false, fmt.Errorf("%w: response row is missing the configured id column", ErrInvalidResponse)
	}
	rowLiteral, err := scalarLiteral(id, false)
	if err != nil || rowLiteral != literal {
		return nil, false, fmt.Errorf("%w: response row does not match the requested key", ErrInvalidResponse)
	}
	return row, true, nil
}

func comparableValue(value any) bool {
	if value == nil {
		return false
	}
	return reflect.ValueOf(value).Comparable()
}

func notSupported(reason string) error {
	return fmt.Errorf("%w: postgrest: %s", dal.ErrNotSupported, reason)
}

func validFieldSource(source, baseAlias, baseName string) bool {
	if source == "" {
		return true
	}
	return source == baseAlias || source == baseName
}
