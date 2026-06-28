// Package immich is a thin read-only client for the Immich REST API. Every
// request carries the x-api-key header; the database is never touched.
package immich

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to one Immich server's REST API under a base URL (including the
// /api prefix).
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// New returns a Client for the given API base URL, API key, and HTTP client.
func New(base, token string, hc *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, hc: hc}
}

// APIError carries the HTTP status so callers can treat "expected" failures
// (403 on admin-only endpoints with a non-admin key, 404 on endpoints missing
// from older Immich versions) as soft rather than aborting the whole scrape.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// StatusIs reports whether err is an APIError with any of the given HTTP codes.
func StatusIs(err error, codes ...int) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	for _, c := range codes {
		if ae.Status == c {
			return true
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return &APIError{method, path, resp.StatusCode, strings.TrimSpace(string(b))}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Get issues a GET and decodes the JSON response into out (nil to discard).
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post issues a JSON POST and decodes the response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// StatTotal counts assets matching a StatisticsSearchDto filter via the cheap
// POST /search/statistics endpoint (returns only {total}). An empty body counts
// everything. Results are scoped to the API key owner's accessible assets.
func (c *Client) StatTotal(ctx context.Context, filter map[string]any) (float64, error) {
	var out struct {
		Total float64 `json:"total"`
	}
	if err := c.Post(ctx, "/search/statistics", filter, &out); err != nil {
		return 0, err
	}
	return out.Total, nil
}

// Suggest returns the distinct values for a search-suggestion type
// (country|state|city|camera-make|camera-model|camera-lens-model).
func (c *Client) Suggest(ctx context.Context, typ string) ([]string, error) {
	var out []string
	if err := c.Get(ctx, "/search/suggestions?type="+url.QueryEscape(typ), &out); err != nil {
		return nil, err
	}
	vals := out[:0]
	for _, v := range out {
		if strings.TrimSpace(v) != "" {
			vals = append(vals, v)
		}
	}
	return vals, nil
}
