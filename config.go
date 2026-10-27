package dalgo2postgrest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultMaxRows          = 1000
	absoluteMaxRows         = 10_000
	defaultMaxPageSize      = 200
	defaultMaxRequests      = 20
	absoluteMaxRequests     = 100
	defaultMaxResponseBytes = 4 << 20
	absoluteMaxResponse     = 64 << 20
	defaultMaxMetadataBytes = 8 << 20
	defaultMaxOffset        = 100_000
	absoluteMaxOffset       = 10_000_000
	defaultMaxGetManyKeys   = 100
	absoluteMaxGetManyKeys  = 1000
	defaultTimeout          = 15 * time.Second
	absoluteMaxTimeout      = 2 * time.Minute
	maxHeaderBytes          = 32 << 10
	maxHeaderCount          = 64
)

// HeaderProvider returns headers for one request. It runs inside the
// operation deadline and receives the operation context, so a credential
// broker can refresh a short-lived Bearer token or read a request-scoped
// value. The adapter may call it concurrently for independent operations, so
// the provider must be safe for concurrent use. Returned values are never
// logged or included in an error.
//
// Authorization and Cookie belong here. Do not put credentials in Endpoint.
type HeaderProvider func(ctx context.Context) (http.Header, error)

// Config configures a read-only PostgREST database.
type Config struct {
	// Endpoint is the PostgREST API root, for example
	// "https://project.supabase.co/rest/v1". It must be HTTPS. Plain HTTP is
	// accepted only for a loopback host when AllowInsecureLoopback is true.
	// Credentials, a query and a fragment are rejected.
	Endpoint string

	// IDColumn is the unique column that carries a DALgo record ID. It defaults
	// to "id". The relation must expose it; queries use it as the stable
	// tie-breaker and Get/Exists filter on it.
	IDColumn string

	// Schema optionally selects a PostgREST schema profile with the
	// Accept-Profile request header. It does not change the URL. Leave empty to
	// use the server's default exposed schema.
	Schema string

	// Headers obtains per-request credentials and other headers. It may be nil.
	Headers HeaderProvider

	// HTTPClient optionally supplies the transport and cookie jar. The adapter
	// clones it, replaces CheckRedirect so redirects are refused, and sets
	// Timeout to the operation deadline. The caller's client is not mutated.
	HTTPClient *http.Client

	// AllowInsecureLoopback permits plain HTTP for localhost, 127.0.0.0/8 and
	// ::1 only. It is a development and test option; leave it false for hosted
	// deployments.
	AllowInsecureLoopback bool

	// MaxRows is the largest positive query limit accepted. Default 1000,
	// maximum 10000.
	MaxRows int

	// MaxPageSize is the largest number of rows requested in one HTTP page.
	// Default 200, never larger than MaxRows.
	MaxPageSize int

	// MaxRequests is the largest number of pages one query may request.
	// Default 20, maximum 100.
	MaxRequests int

	// MaxResponseBytes is the total response-byte budget of one operation.
	// Default 4 MiB, maximum 64 MiB.
	MaxResponseBytes int64

	// MaxMetadataBytes is the byte budget of one OpenAPI document. Default
	// 8 MiB, maximum 64 MiB.
	MaxMetadataBytes int64

	// MaxOffset is the largest query offset accepted. Default 100000, maximum
	// 10000000.
	MaxOffset int

	// MaxGetManyKeys is the largest number of records accepted by GetMulti.
	// Default 100, maximum 1000.
	MaxGetManyKeys int

	// Timeout is the single deadline of one operation, covering header
	// retrieval, every HTTP exchange, body reading and all pages. Default 15s,
	// maximum 2m.
	Timeout time.Duration
}

type config struct {
	base             *url.URL
	idColumn         string
	schema           string
	headers          HeaderProvider
	client           *http.Client
	maxRows          int
	maxPageSize      int
	maxRequests      int
	maxResponseBytes int64
	maxMetadataBytes int64
	maxOffset        int
	maxGetManyKeys   int
	timeout          time.Duration
}

func validateConfig(ctx context.Context, cfg Config) (config, error) {
	if ctx == nil {
		return config{}, invalid("a context is required")
	}
	if err := ctx.Err(); err != nil {
		return config{}, err
	}

	base, err := endpointURL(cfg.Endpoint, cfg.AllowInsecureLoopback)
	if err != nil {
		return config{}, err
	}

	idColumn := strings.TrimSpace(cfg.IDColumn)
	if idColumn == "" {
		idColumn = "id"
	}
	if !validIdentifier(idColumn) {
		return config{}, invalid("id column must be a simple SQL identifier of at most 63 bytes")
	}

	schema := strings.TrimSpace(cfg.Schema)
	if schema != "" && !validIdentifier(schema) {
		return config{}, invalid("schema must be a simple SQL identifier of at most 63 bytes")
	}

	maxRows, err := boundedInt(cfg.MaxRows, defaultMaxRows, 1, absoluteMaxRows, "max rows")
	if err != nil {
		return config{}, err
	}
	pageDefault := defaultMaxPageSize
	if pageDefault > maxRows {
		pageDefault = maxRows
	}
	maxPageSize, err := boundedInt(cfg.MaxPageSize, pageDefault, 1, maxRows, "max page size")
	if err != nil {
		return config{}, err
	}
	maxRequests, err := boundedInt(cfg.MaxRequests, defaultMaxRequests, 1, absoluteMaxRequests, "max requests")
	if err != nil {
		return config{}, err
	}
	maxResponseBytes, err := boundedBytes(cfg.MaxResponseBytes, defaultMaxResponseBytes, absoluteMaxResponse, "max response bytes")
	if err != nil {
		return config{}, err
	}
	maxMetadataBytes, err := boundedBytes(cfg.MaxMetadataBytes, defaultMaxMetadataBytes, absoluteMaxResponse, "max metadata bytes")
	if err != nil {
		return config{}, err
	}
	maxOffset, err := boundedInt(cfg.MaxOffset, defaultMaxOffset, 0, absoluteMaxOffset, "max offset")
	if err != nil {
		return config{}, err
	}
	maxGetManyKeys, err := boundedInt(cfg.MaxGetManyKeys, defaultMaxGetManyKeys, 1, absoluteMaxGetManyKeys, "max getMany keys")
	if err != nil {
		return config{}, err
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout < time.Millisecond || timeout > absoluteMaxTimeout {
		return config{}, invalid("timeout must be between 1ms and 2m")
	}

	client := &http.Client{Transport: http.DefaultTransport}
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		client = &clone
	}
	if client.Transport == nil {
		client.Transport = http.DefaultTransport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Timeout = timeout

	return config{
		base:             base,
		idColumn:         idColumn,
		schema:           schema,
		headers:          cfg.Headers,
		client:           client,
		maxRows:          maxRows,
		maxPageSize:      maxPageSize,
		maxRequests:      maxRequests,
		maxResponseBytes: maxResponseBytes,
		maxMetadataBytes: maxMetadataBytes,
		maxOffset:        maxOffset,
		maxGetManyKeys:   maxGetManyKeys,
		timeout:          timeout,
	}, nil
}

func endpointURL(raw string, allowLoopback bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, invalid("endpoint is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, invalid("endpoint must be an absolute URL without credentials, a query or a fragment")
	}
	https := strings.EqualFold(u.Scheme, "https")
	httpLoopback := strings.EqualFold(u.Scheme, "http") && allowLoopback && isLoopbackHost(u.Hostname())
	if !https && !httpLoopback {
		return nil, invalid("endpoint must use HTTPS; plain HTTP is allowed only for a loopback host when AllowInsecureLoopback is set")
	}
	if strings.ContainsRune(u.Path, '\\') || unsafePath(u.Path) || unsafePath(u.EscapedPath()) {
		return nil, invalid("endpoint path contains an unsafe segment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "/" {
		u.Path = ""
	}
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u, nil
}

func unsafePath(path string) bool {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2e") {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func boundedInt(value, fallback, minimum, maximum int, label string) (int, error) {
	if value == 0 {
		value = fallback
	}
	if value < minimum || value > maximum {
		return 0, invalid(fmt.Sprintf("%s must be from %d to %d", label, minimum, maximum))
	}
	return value, nil
}

func boundedBytes(value, fallback, maximum int64, label string) (int64, error) {
	if value == 0 {
		value = fallback
	}
	if value < 1 || value > maximum {
		return 0, invalid(fmt.Sprintf("%s must be from 1 to %d", label, maximum))
	}
	return value, nil
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidConfig, reason)
}

func validIdentifier(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		letter := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
		if i == 0 {
			if !letter {
				return false
			}
			continue
		}
		if !letter && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

func validateHeaders(headers http.Header) error {
	if len(headers) > maxHeaderCount {
		return fmt.Errorf("%w: too many headers", ErrUnsafeHeader)
	}
	total := 0
	for name, values := range headers {
		if !validHeaderName(name) {
			return fmt.Errorf("%w: invalid header name", ErrUnsafeHeader)
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "transfer-encoding", "connection", "accept", "accept-profile",
			"content-profile", "prefer", "range", "range-unit", "te", "trailer", "upgrade", "proxy-authorization":
			return fmt.Errorf("%w: %s is owned by the adapter or is hop-by-hop", ErrUnsafeHeader, name)
		}
		for _, value := range values {
			if strings.ContainsAny(value, "\r\n\x00") {
				return fmt.Errorf("%w: header value contains a control character", ErrUnsafeHeader)
			}
			total += len(name) + len(value)
		}
	}
	if total > maxHeaderBytes {
		return fmt.Errorf("%w: headers exceed %d bytes", ErrUnsafeHeader, maxHeaderBytes)
	}
	return nil
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
