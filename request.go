package dalgo2postgrest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// getBytes performs one bounded GET and returns the response body. It is the
// body-only convenience wrapper around getPayload.
func (d *Database) getBytes(ctx context.Context, op string, target *url.URL, accept string, maxBytes int64) ([]byte, error) {
	payload, err := d.getPayload(ctx, op, target, accept, maxBytes)
	if err != nil {
		return nil, err
	}
	return payload.body, nil
}

// httpPayload is one complete, bounded HTTP response. The header is retained so
// a paged read can validate PostgREST's Content-Range instead of guessing that
// a short page means end of data.
type httpPayload struct {
	body   []byte
	header http.Header
}

// getPayload performs one bounded GET. Every failure that could contain a URL,
// token or response body is replaced by a redacted sentinel error.
func (d *Database) getPayload(ctx context.Context, op string, target *url.URL, accept string, maxBytes int64) (httpPayload, error) {
	if err := d.closedErr(); err != nil {
		return httpPayload{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return httpPayload{}, err
	}
	headers, err := d.requestHeaders(ctx)
	if err != nil {
		return httpPayload{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return httpPayload{}, fmt.Errorf("%w: %s request could not be built", ErrRequestFailed, op)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Accept", accept)
	if d.cfg.schema != "" {
		request.Header.Set("Accept-Profile", d.cfg.schema)
	}

	response, err := d.cfg.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return httpPayload{}, ctxErr
		}
		if isContextError(err) {
			return httpPayload{}, ErrRequestFailed
		}
		return httpPayload{}, fmt.Errorf("%w: %s request could not be completed", ErrRequestFailed, op)
	}
	defer func() { _ = response.Body.Close() }()

	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return httpPayload{}, fmt.Errorf("%w: HTTP %d", ErrRedirectRefused, response.StatusCode)
	case response.StatusCode == http.StatusUnauthorized, response.StatusCode == http.StatusForbidden:
		return httpPayload{}, &HTTPError{StatusCode: response.StatusCode}
	case response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent:
		return httpPayload{}, &HTTPError{StatusCode: response.StatusCode}
	}

	mediaType := response.Header.Get("Content-Type")
	parsed, _, parseErr := mime.ParseMediaType(mediaType)
	if parseErr != nil || !jsonMediaType(parsed) {
		return httpPayload{}, fmt.Errorf("%w: unexpected content type", ErrInvalidResponse)
	}
	if response.ContentLength > maxBytes {
		return httpPayload{}, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return httpPayload{}, ctxErr
		}
		return httpPayload{}, fmt.Errorf("%w: response body is truncated or unreadable", ErrInvalidResponse)
	}
	if int64(len(body)) > maxBytes {
		return httpPayload{}, ErrResponseTooLarge
	}
	if len(body) == 0 {
		return httpPayload{}, fmt.Errorf("%w: empty response body", ErrInvalidResponse)
	}
	return httpPayload{body: body, header: response.Header.Clone()}, nil
}

func (d *Database) requestHeaders(ctx context.Context) (http.Header, error) {
	if d.cfg.headers == nil {
		return nil, nil
	}
	type result struct {
		headers http.Header
		err     error
	}
	done := make(chan result, 1)
	go func() {
		headers, err := d.cfg.headers(ctx)
		done <- result{headers: headers, err: err}
	}()
	var headers http.Header
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case provided := <-done:
		if provided.err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("%w: authentication headers could not be obtained", ErrRequestFailed)
		}
		headers = provided.headers
	}
	if err := validateHeaders(headers); err != nil {
		return nil, err
	}
	return headers, nil
}

func jsonMediaType(mediaType string) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func decodeRows(body []byte, maxRows int) ([]map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: body is not valid JSON", ErrInvalidResponse)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("%w: body holds more than one JSON value", ErrInvalidResponse)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: body is not a JSON array of row objects", ErrInvalidResponse)
	}
	if len(items) > maxRows {
		return nil, fmt.Errorf("%w: body returned %d rows where at most %d were requested", ErrInvalidResponse, len(items), maxRows)
	}
	rows := make([]map[string]any, len(items))
	for i, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%w: row %d is not a JSON object", ErrInvalidResponse, i)
		}
		normalized, err := normalizeNumbers(row, 0)
		if err != nil {
			return nil, err
		}
		rows[i] = normalized.(map[string]any)
	}
	return rows, nil
}

// numberValue preserves JSON integers that fit int64 exactly. Everything else
// stays a json.Number, so a decimal or an integer beyond int64 never passes
// through float64 and loses digits. json.Number marshals back as a JSON number
// and record.MapToData can still fill a float or decimal struct field from it.
func numberValue(value json.Number) (any, error) {
	text := value.String()
	if !reJSONNumber.MatchString(text) {
		return nil, fmt.Errorf("%w: invalid JSON number", ErrInvalidResponse)
	}
	if !strings.ContainsAny(text, ".eE") {
		if integer, err := strconv.ParseInt(text, 10, 64); err == nil {
			return integer, nil
		}
	}
	return value, nil
}

func normalizeNumbers(value any, depth int) (any, error) {
	if depth > 32 {
		return nil, fmt.Errorf("%w: JSON nesting is deeper than 32 levels", ErrInvalidResponse)
	}
	switch typed := value.(type) {
	case json.Number:
		return numberValue(typed)
	case map[string]any:
		for key, item := range typed {
			normalized, err := normalizeNumbers(item, depth+1)
			if err != nil {
				return nil, err
			}
			typed[key] = normalized
		}
		return typed, nil
	case []any:
		for i, item := range typed {
			normalized, err := normalizeNumbers(item, depth+1)
			if err != nil {
				return nil, err
			}
			typed[i] = normalized
		}
		return typed, nil
	default:
		return value, nil
	}
}
