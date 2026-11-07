// Package github is a minimal read-only GitHub REST client for zerobill's collectors.
package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// Client issues authenticated GET requests against the GitHub REST API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	token   string
}

// New returns a client for api.github.com. The token is sent only in the
// Authorization header and never appears in errors.
func New(token string) *Client {
	return &Client{BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}, token: token}
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Get decodes the JSON response of path, relative to BaseURL, into out.
func (c *Client) Get(path string, out any) error {
	_, err := c.get(c.BaseURL+path, out)
	return err
}

// ErrStop, returned by a Paginate page callback, ends pagination without an error.
var ErrStop = errors.New("stop pagination")

// Paginate calls page with each response body of path until no next link remains
// or page returns ErrStop.
func (c *Client) Paginate(path string, page func(body []byte) error) error {
	u := c.BaseURL + path
	for u != "" {
		var raw json.RawMessage
		next, err := c.get(u, &raw)
		if err != nil {
			return err
		}
		if err := page(raw); errors.Is(err, ErrStop) {
			return nil
		} else if err != nil {
			return fmt.Errorf("GET %s: %w", redact(u), err)
		}
		if next != "" && !sameHost(next, c.BaseURL) {
			return fmt.Errorf("GET %s: next page link leaves %s", redact(u), c.BaseURL)
		}
		u = next
	}
	return nil
}

func (c *Client) get(u string, out any) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "zerobill")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", redact(u), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", redact(u), err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		return "", fmt.Errorf("GET %s: %s: %s", redact(u), resp.Status, e.Message)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return "", fmt.Errorf("GET %s: %w", redact(u), err)
	}
	var next string
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		next = m[1]
	}
	return next, nil
}

// redact drops the scheme and host so errors stay short and stable.
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	return p.RequestURI()
}

// sameHost keeps the token from following a pagination link to another host.
func sameHost(a, b string) bool {
	pa, errA := url.Parse(a)
	pb, errB := url.Parse(b)
	return errA == nil && errB == nil && pa.Scheme == pb.Scheme && pa.Host == pb.Host
}
