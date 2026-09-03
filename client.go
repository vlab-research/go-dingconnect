// Package dingconnect is a client for the DingConnect mobile top-up API.
//
// It has no dependencies outside the standard library. Every call takes a
// context and returns a typed value plus an error; application-level failures
// arrive as *Error, which carries DingConnect's ResultCode and ErrorCodes.
//
//	c := dingconnect.New(os.Getenv("DINGCONNECT_API_KEY"))
//	bal, err := c.Balance(ctx)
//
// Wire contract notes, all verified against the live API:
//
//   - Authentication is the header "api_key". It is NOT "X-Api-Key",
//     "Authorization", or a bearer token; those yield HTTP 401 with
//     ResultCode 4 / AuthenticationFailed.
//   - Every field on the wire is PascalCase ("SkuCode", "ResultCode",
//     "CurrencyIso"). snake_case is silently ignored on requests and decodes
//     to zero values on responses.
//   - Application errors come back as HTTP 200 with a non-1 ResultCode, so the
//     HTTP status alone never tells you whether a call succeeded.
package dingconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the production DingConnect API root. There is no sandbox
// environment; use SendTransferRequest.ValidateOnly to exercise the transfer
// path without moving money.
const DefaultBaseURL = "https://api.dingconnect.com/api/V1"

// DefaultTimeout bounds a single request. Transfers are the slow path and can
// legitimately take tens of seconds while the provider is contacted.
const DefaultTimeout = 90 * time.Second

// Client is a DingConnect API client. It is safe for concurrent use.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client

	// Catalogue cache, used only by Pay. Held per Client because it is
	// therefore per API key, and commission rates -- so what a SKU actually
	// delivers -- are a property of the distributor account.
	catalogueTTL time.Duration
	cacheOnce    sync.Once
	cache        *catalogueCache
}

// Option customises a Client.
type Option func(*Client)

// WithBaseURL overrides the API root. Used by tests to point at an httptest
// server.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient supplies the underlying *http.Client, letting callers control
// timeouts, transports, and instrumentation.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New returns a Client authenticating with the given API key.
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		apiKey:       apiKey,
		baseURL:      DefaultBaseURL,
		http:         &http.Client{Timeout: DefaultTimeout},
		catalogueTTL: DefaultCatalogueTTL,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// do performs one API call and decodes the response into T.
//
// It returns the decoded value even when err is non-nil, provided a body was
// successfully parsed. SendTransfer relies on this: a declined transfer still
// carries a TransferRecord worth recording.
func do[T statusHolder](ctx context.Context, c *Client, method, path string, query url.Values, body any) (T, error) {
	var out T

	fail := func(e *Error) (T, error) {
		e.Method, e.Path = method, path
		return out, e
	}

	if c.apiKey == "" {
		return fail(&Error{Err: fmt.Errorf("no API key configured")})
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fail(&Error{Err: fmt.Errorf("encoding request: %w", err)})
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fail(&Error{Err: fmt.Errorf("building request: %w", err)})
	}

	// "api_key" is the only header DingConnect accepts for authentication.
	req.Header.Set("api_key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fail(&Error{Err: fmt.Errorf("request failed: %w", err)})
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(&Error{StatusCode: resp.StatusCode, Err: fmt.Errorf("reading response: %w", err)})
	}

	if err := json.Unmarshal(raw, &out); err != nil {
		return fail(&Error{
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("decoding response: %w (body: %s)", err, truncate(raw, 256)),
		})
	}

	st := out.status()
	if st.ResultCode != ResultCodeSuccess {
		// out is returned populated alongside the error on purpose.
		return out, &Error{
			ResultCode: st.ResultCode,
			Codes:      st.ErrorCodes,
			StatusCode: resp.StatusCode,
			Method:     method,
			Path:       path,
		}
	}

	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// List is the shape of every collection-returning DingConnect endpoint.
type List[T any] struct {
	Status
	Items []T `json:"Items"`
}

// query builds a url.Values, repeating a key once per value as DingConnect
// expects for its array parameters (countryIsos=NG&countryIsos=KE).
func query(pairs ...any) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		key, _ := pairs[i].(string)
		switch val := pairs[i+1].(type) {
		case string:
			if val != "" {
				v.Add(key, val)
			}
		case []string:
			for _, s := range val {
				if s != "" {
					v.Add(key, s)
				}
			}
		case bool:
			if val {
				v.Add(key, "true")
			}
		case int:
			if val != 0 {
				v.Add(key, fmt.Sprint(val))
			}
		}
	}
	return v
}
