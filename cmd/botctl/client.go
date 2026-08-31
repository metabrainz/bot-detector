package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// defaultBaseURL is used when neither --url nor BOT_DETECTOR_URL is set.
const defaultBaseURL = "http://localhost:8090"

// envURL is the environment variable that provides the target base URL.
const envURL = "BOT_DETECTOR_URL"

// client wraps an HTTP client bound to a bot-detector base URL.
type client struct {
	baseURL string
	http    *http.Client
}

// resolveBaseURL determines the target base URL from the explicit flag, then
// the environment, then the built-in default. The result never has a trailing
// slash so paths can be joined directly.
func resolveBaseURL(flagURL string) string {
	raw := flagURL
	if raw == "" {
		raw = os.Getenv(envURL)
	}
	if raw == "" {
		raw = defaultBaseURL
	}
	return strings.TrimRight(raw, "/")
}

// newClient builds a client for the given base URL and timeout.
func newClient(baseURL string, timeout time.Duration) *client {
	return &client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout},
	}
}

// httpError carries an unexpected HTTP status and the response body for
// diagnostics.
type httpError struct {
	status int
	body   string
}

func (e *httpError) Error() string {
	body := strings.TrimSpace(e.body)
	if body == "" {
		return fmt.Sprintf("server returned HTTP %d", e.status)
	}
	return fmt.Sprintf("server returned HTTP %d: %s", e.status, body)
}

// do performs a request against path (which must begin with "/") and returns
// the raw response body. A non-2xx status yields an *httpError.
func (c *client) do(method, path string, query url.Values) ([]byte, error) {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, &httpError{status: resp.StatusCode, body: string(body)}
	}
	return body, nil
}

// doJSON performs a request and decodes a JSON response into out.
// It also returns the raw body so callers can pass it through with --json.
func (c *client) doJSON(method, path string, query url.Values, out any) ([]byte, error) {
	body, err := c.do(method, path, query)
	if err != nil {
		return body, err
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return body, fmt.Errorf("decode JSON response: %w", err)
		}
	}
	return body, nil
}
