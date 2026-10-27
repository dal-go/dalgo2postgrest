package dalgo2postgrest

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/dalgo/recordset"
	"github.com/dal-go/record"
)

type rowPager struct {
	db        *Database
	ctx       context.Context
	plan      queryPlan
	offset    int
	remaining int
	bytesUsed int64
	requests  int
	finished  bool
}

func newRowPager(ctx context.Context, db *Database, plan queryPlan) *rowPager {
	return &rowPager{
		db:        db,
		ctx:       ctx,
		plan:      plan,
		offset:    plan.offset,
		remaining: plan.limit,
	}
}

// nextPage reads one bounded PostgREST page. done reports that the query has no
// further rows. The pager owns the total request and byte budgets across pages.
//
// A page shorter than the requested size is not treated as end of data:
// PostgREST can shorten a page because of its server-side db-max-rows setting.
// The pager therefore continues from the next offset and only finishes on an
// empty page, a known Content-Range total, or the caller's row limit. If the
// configured request budget cannot cover those continuation pages, the read
// fails with ErrBudgetExceeded instead of returning truncated data.
func (p *rowPager) nextPage() (rows []map[string]any, done bool, err error) {
	if p.finished || p.remaining == 0 {
		return nil, true, nil
	}
	if ctxErr := p.ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	if err = p.db.closedErr(); err != nil {
		return nil, false, err
	}
	if p.requests >= p.db.cfg.maxRequests {
		return nil, false, fmt.Errorf("%w: the query needs more than %d requests; a shortened page cannot be verified as complete", ErrBudgetExceeded, p.db.cfg.maxRequests)
	}
	pageSize := p.plan.pageSize
	if p.remaining < pageSize {
		pageSize = p.remaining
	}
	budget := p.db.cfg.maxResponseBytes - p.bytesUsed
	if budget <= 0 {
		return nil, false, ErrResponseTooLarge
	}
	params := cloneValues(p.plan.params)
	params.Set("limit", strconv.Itoa(pageSize))
	params.Set("offset", strconv.Itoa(p.offset))
	p.requests++
	payload, err := p.db.getPayload(p.ctx, "query", p.db.relationURL(p.plan.collection, params), "application/json", budget)
	if err != nil {
		return nil, false, err
	}
	p.bytesUsed += int64(len(payload.body))
	page, err := decodeRows(payload.body, pageSize)
	if err != nil {
		return nil, false, err
	}
	total, err := validateContentRange(payload.header.Get("Content-Range"), p.offset, len(page))
	if err != nil {
		return nil, false, err
	}
	p.offset += len(page)
	p.remaining -= len(page)
	if len(page) == 0 {
		p.finished = true
	}
	if total != nil && p.offset >= int(*total) {
		p.finished = true
	}
	return page, p.finished || p.remaining == 0, nil
}

// validateContentRange checks PostgREST's Content-Range against the page that
// was requested. A missing header is tolerated: continuation still requires a
// further request and an empty page before the read succeeds, so a missing
// header cannot silently truncate a result. A present but malformed or
// inconsistent header is rejected: it could otherwise hide a repeated or
// out-of-order page.
func validateContentRange(value string, offset, count int) (*int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return nil, invalidContentRange()
	}
	var total *int64
	switch totalText := strings.TrimSpace(parts[1]); totalText {
	case "*":
	case "":
		return nil, invalidContentRange()
	default:
		parsed, err := strconv.ParseInt(totalText, 10, 64)
		if err != nil || parsed < 0 {
			return nil, invalidContentRange()
		}
		total = &parsed
	}
	rangeText := strings.TrimSpace(parts[0])
	if rangeText == "*" {
		if count != 0 {
			return nil, invalidContentRange()
		}
		if total != nil && int64(offset) < *total {
			return nil, invalidContentRange()
		}
		return total, nil
	}
	bounds := strings.Split(rangeText, "-")
	if len(bounds) != 2 {
		return nil, invalidContentRange()
	}
	start, startErr := strconv.ParseInt(strings.TrimSpace(bounds[0]), 10, 64)
	end, endErr := strconv.ParseInt(strings.TrimSpace(bounds[1]), 10, 64)
	if startErr != nil || endErr != nil || start < 0 || end < start {
		return nil, invalidContentRange()
	}
	if count == 0 {
		return nil, invalidContentRange()
	}
	if start != int64(offset) || end-start+1 != int64(count) {
		return nil, invalidContentRange()
	}
	if total != nil && end >= *total {
		return nil, invalidContentRange()
	}
	return total, nil
}

func invalidContentRange() error {
	return fmt.Errorf("%w: Content-Range does not describe the requested page", ErrInvalidResponse)
}

func cloneValues(values url.Values) url.Values {
	clone := make(url.Values, len(values))
	for key, items := range values {
		clone[key] = append([]string(nil), items...)
	}
	return clone
}

var _ dal.RecordsReader = (*recordsReader)(nil)

type recordsReader struct {
	pager  *rowPager
	cancel context.CancelFunc
	rows   []record.Record
	index  int
	err    error
	closed atomic.Bool
}

func (r *recordsReader) Next() (record.Record, error) {
	if r.closed.Load() {
		return nil, dal.ErrReaderClosed
	}
	if r.err != nil {
		return nil, r.err
	}
	// Recheck before every returned row, not only before a page is fetched. Rows
	// already buffered from an earlier page must not outlive a caller abort, a
	// closed database or an expired operation deadline. The check also runs
	// after a page has been built: building a row invokes the caller's
	// IntoRecord callback, which may cancel the context or close the database
	// itself.
	for {
		if err := r.terminalError(); err != nil {
			return r.fail(err)
		}
		if r.index < len(r.rows) {
			rec := r.rows[r.index]
			r.index++
			return rec, nil
		}
		rows, done, err := r.pager.nextPage()
		if err != nil {
			return r.fail(err)
		}
		if len(rows) > 0 {
			built := make([]record.Record, len(rows))
			for i, row := range rows {
				rec, buildErr := r.buildRecord(row)
				if buildErr != nil {
					return r.fail(buildErr)
				}
				built[i] = rec
			}
			r.rows = built
			r.index = 0
			continue
		}
		if done {
			// Cache the terminal result. The operation context is canceled as
			// part of releasing resources, so without this a later Next would
			// surface context.Canceled instead of the stable EOF contract.
			r.err = dal.ErrNoMoreRecords
			r.cancel()
			return nil, r.err
		}
	}
}

// terminalError is the first sticky terminal condition of the reader. It lets
// every later Next return the same error instead of one more buffered row.
func (r *recordsReader) terminalError() error {
	if ctxErr := r.pager.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return r.pager.db.closedErr()
}

func (r *recordsReader) fail(err error) (record.Record, error) {
	r.err = err
	r.rows = nil
	r.index = 0
	r.cancel()
	return nil, err
}

func (r *recordsReader) buildRecord(row map[string]any) (record.Record, error) {
	id, ok := row[r.pager.db.cfg.idColumn]
	if !ok || id == nil {
		return nil, fmt.Errorf("%w: response row is missing the configured id column", ErrInvalidResponse)
	}
	if !comparableValue(id) {
		return nil, fmt.Errorf("%w: response row id is not a comparable scalar", ErrInvalidResponse)
	}
	data, err := r.pager.plan.projectRow(row)
	if err != nil {
		return nil, err
	}
	key := record.NewKeyWithID(r.pager.plan.collection, id)
	if r.pager.plan.template == nil {
		return record.NewRecordWithData(key, data), nil
	}
	template := r.pager.plan.template()
	if template == nil {
		return record.NewRecordWithData(key, data), nil
	}
	template.SetError(nil)
	target := template.Data()
	if target == nil {
		return record.NewRecordWithData(key, data), nil
	}
	if err = record.MapToData(target, data); err != nil {
		return nil, err
	}
	return record.NewRecordWithData(key, target), nil
}

// projectRow restricts a response row to the output columns the query declared.
// A wildcard plan (projected == nil) keeps the whole response row. An explicit
// projection is honored exactly: an extra response field must not leak into the
// record's data, and a declared field missing from the response is a protocol
// violation rather than a silently dropped column. The hidden id column added
// for the record key is not part of projected, so it never leaks either.
func (p queryPlan) projectRow(row map[string]any) (map[string]any, error) {
	if p.projected == nil {
		return row, nil
	}
	data := make(map[string]any, len(p.projected))
	for _, name := range p.projected {
		value, found := row[name]
		if !found {
			return nil, fmt.Errorf("%w: response row is missing the projected column %q", ErrInvalidResponse, name)
		}
		data[name] = value
	}
	return data, nil
}

func (r *recordsReader) Cursor() (string, error) {
	return "", dal.ErrNotImplementedYet
}

func (r *recordsReader) Close() error {
	if r.closed.CompareAndSwap(false, true) {
		r.cancel()
	}
	return nil
}

func (b *backend) ExecuteQueryToRecordsReader(ctx context.Context, query dal.Query) (dal.RecordsReader, error) {
	if err := b.db.closedErr(); err != nil {
		return nil, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	plan, err := makePlan(query, b.db.cfg)
	if err != nil {
		return nil, err
	}
	opCtx, cancel := b.db.operationContext(ctx)
	return &recordsReader{
		pager:  newRowPager(opCtx, b.db, plan),
		cancel: cancel,
	}, nil
}

var _ dal.RecordsetReader = (*recordsetReader)(nil)

type recordsetReader struct {
	rs       recordset.Recordset
	position int
	ctx      context.Context
	db       *Database
	err      error
	closed   atomic.Bool
}

func (r *recordsetReader) Recordset() recordset.Recordset {
	return r.rs
}

func (r *recordsetReader) Cursor() (string, error) {
	return "", dal.ErrNotImplementedYet
}

func (r *recordsetReader) Close() error {
	r.closed.Store(true)
	return nil
}

func (r *recordsetReader) Next() (recordset.Row, recordset.Recordset, error) {
	if r.closed.Load() {
		return nil, r.rs, dal.ErrReaderClosed
	}
	if r.err != nil {
		return nil, r.rs, r.err
	}
	// The recordset rows are already materialized, but a caller abort or a
	// closed database still has to stop iteration. Cache the first terminal
	// error so later calls do not resume after an abort.
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			r.err = err
			return nil, r.rs, err
		}
	}
	if err := r.db.closedErr(); err != nil {
		r.err = err
		return nil, r.rs, err
	}
	if r.position >= r.rs.RowsCount() {
		r.err = dal.ErrNoMoreRecords
		return nil, r.rs, r.err
	}
	row := r.rs.GetRow(r.position)
	r.position++
	return row, r.rs, nil
}

func (b *backend) ExecuteQueryToRecordsetReader(ctx context.Context, query dal.Query, options ...recordset.Option) (dal.RecordsetReader, error) {
	if err := b.db.closedErr(); err != nil {
		return nil, err
	}
	callerCtx := ctx
	if callerCtx == nil {
		callerCtx = context.Background()
	}
	if err := callerCtx.Err(); err != nil {
		return nil, err
	}
	plan, err := makePlan(query, b.db.cfg)
	if err != nil {
		return nil, err
	}
	opCtx, cancel := b.db.operationContext(callerCtx)
	defer cancel()
	pager := newRowPager(opCtx, b.db, plan)
	var rows []map[string]any
	for {
		page, done, err := pager.nextPage()
		if err != nil {
			return nil, err
		}
		for _, row := range page {
			if plan.projected == nil {
				// A wildcard read keeps every response field; its columns are
				// derived from the rows after all pages have arrived.
				rows = append(rows, row)
				continue
			}
			// Enforce the same declared-output contract as the records reader
			// before materialization. Without this check a declared column that
			// PostgREST did not return would be stored as nil, making an absent
			// column indistinguishable from an explicit SQL NULL.
			projectedRow, projectErr := plan.projectRow(row)
			if projectErr != nil {
				return nil, projectErr
			}
			rows = append(rows, projectedRow)
		}
		if done {
			break
		}
	}
	columns := plan.projected
	if columns == nil {
		seen := make(map[string]bool)
		for _, row := range rows {
			for name := range row {
				seen[name] = true
			}
		}
		columns = make([]string, 0, len(seen))
		for name := range seen {
			columns = append(columns, name)
		}
		sort.Strings(columns)
	}
	definitions := make([]recordset.Column[any], len(columns))
	for i, name := range columns {
		definitions[i] = recordset.NewTypedColumn[any](name, nil)
	}
	name := plan.collection
	if configured := recordset.NewOptions(options...).Name(); configured != "" {
		name = configured
	}
	rs := recordset.NewColumnarRecordset(name, definitions...)
	for _, source := range rows {
		row := rs.NewRow()
		for _, column := range columns {
			_ = row.SetValueByName(column, source[column], rs)
		}
	}
	return &recordsetReader{rs: rs, ctx: callerCtx, db: b.db}, nil
}
