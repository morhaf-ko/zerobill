// Package github is a minimal GitHub REST client for zerobill's collectors and
// variable writer.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Client issues authenticated requests against the GitHub REST API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	token   string
	ctx     context.Context
	backoff *backoff
	pages   *pageCache
	// Progress receives the request path before I/O, without query parameters or credentials.
	Progress func(string)
	// ObserveRequest receives sanitized transport results and may be called concurrently.
	ObserveRequest func(RequestObservation)
}

// New returns a client for api.github.com. The token is sent only in the
// Authorization header and never appears in errors.
func New(token string) *Client {
	return &Client{BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 30 * time.Second}, token: token}
}

// WithContext bounds an operation without changing the authenticated client.
func (c *Client) WithContext(ctx context.Context) *Client {
	scoped := *c
	scoped.ctx = ctx
	return &scoped
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// Get decodes the JSON response of path, relative to BaseURL, into out.
func (c *Client) Get(path string, out any) error {
	_, err := c.do(http.MethodGet, c.BaseURL+path, nil, out, "", "")
	return err
}

// ConditionalGet revalidates a previously observed resource when enabled.
func (c *Client) ConditionalGet(path string, out any) error {
	u := c.BaseURL + path
	var root string
	if c.pages != nil {
		root = u
	}
	_, err := c.do(http.MethodGet, u, nil, out, "", root)
	return err
}

// ErrStop, returned by a Paginate page callback, ends pagination without an error.
var ErrStop = errors.New("stop pagination")

// Paginate calls page with each response body of path until no next link remains
// or page returns ErrStop.
func (c *Client) Paginate(path string, page func(body []byte) error) error {
	return c.paginate(path, page, false)
}

// ConditionalPaginate revalidates each page linked by the current response.
func (c *Client) ConditionalPaginate(path string, page func(body []byte) error) error {
	return c.paginate(path, page, c.pages != nil)
}

func (c *Client) paginate(path string, page func(body []byte) error, conditional bool) error {
	u := c.BaseURL + path
	var root string
	seen := make(map[string]bool)
	if conditional {
		root = u
	}
	var endpoint string
	if conditional || c.ObserveRequest != nil {
		if first, err := url.Parse(u); err == nil {
			endpoint = endpointClass(first)
		}
	}
	for u != "" {
		if seen[u] {
			return fmt.Errorf("GET %s: pagination repeats a page", redact(u))
		}
		var raw json.RawMessage
		next, err := c.do(http.MethodGet, u, nil, &raw, endpoint, root)
		if err != nil {
			return err
		}
		err = page(raw)
		if err == nil || errors.Is(err, ErrStop) {
			seen[u] = true
		}
		if errors.Is(err, ErrStop) {
			if conditional {
				c.pages.prune(root, seen)
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("GET %s: %w", redact(u), err)
		}
		if next != "" && !sameHost(next, c.BaseURL) {
			return fmt.Errorf("GET %s: next page link leaves %s", redact(u), c.BaseURL)
		}
		u = next
	}
	if conditional {
		c.pages.prune(root, seen)
	}
	return nil
}

// StatusError is a response with a status other than 2xx.
type StatusError struct {
	Method, Path, Status, Message string
	Code                          int
	RateLimited                   bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 response.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

func IsRateLimited(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.RateLimited && se.Code != http.StatusUnauthorized
}

// IsAccessDenied excludes rate limits, which re-authenticating cannot fix.
func IsAccessDenied(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || (se.Code == http.StatusForbidden && !se.RateLimited))
}

// Send issues a write request with a JSON body to path, relative to BaseURL.
func (c *Client) Send(method, path string, body any) error {
	u := c.BaseURL + path
	if c.pages != nil {
		c.pages.invalidate(u)
	}
	_, err := c.do(method, u, body, nil, "", "")
	return err
}

func (c *Client) do(method, u string, in, out any, endpoint, root string) (string, error) {
	if c.backoff != nil && time.Now().Before(c.backoff.until) {
		return "", &StatusError{Method: method, Path: redact(u), Status: "rate limited",
			Message: "backoff until " + c.backoff.until.UTC().Format(time.RFC3339), Code: http.StatusTooManyRequests, RateLimited: true}
	}
	var reqBody io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		reqBody = bytes.NewReader(raw)
	}
	ctx := c.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return "", err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "zerobill")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	var cached cachedPage
	var match bool
	key := pageKey{root: root, url: u}
	if root != "" {
		cached, match = c.pages.get(key)
		if match {
			req.Header.Set("If-None-Match", cached.etag)
		}
	}
	if c.Progress != nil {
		c.Progress(req.URL.Path)
	}
	if endpoint == "" && (c.ObserveRequest != nil || root != "") {
		endpoint = endpointClass(req.URL)
	}
	client := c.HTTP
	if c.ObserveRequest != nil {
		wrapped := *client
		transport := wrapped.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		wrapped.Transport = observedTransport{transport, endpoint, c.ObserveRequest}
		client = &wrapped
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, redact(u), err)
	}
	defer resp.Body.Close()
	var next string
	link := resp.Header.Get("Link")
	if m := nextLink.FindStringSubmatch(link); m != nil {
		next = m[1]
	}
	etag := resp.Header.Get("ETag")
	if c.backoff != nil && (resp.StatusCode == http.StatusNotModified || resp.StatusCode/100 == 2) {
		c.backoff.succeeded(u)
	}
	finalURL := u
	if resp.Request != nil {
		finalURL = resp.Request.URL.String()
	}
	if resp.StatusCode == http.StatusNotModified && root != "" {
		if !match || finalURL != cached.finalURL || (etag != "" && etag != cached.etag) {
			c.pages.remove(c.pages.pages[key])
			resp.Body.Close()
			return c.do(method, u, in, out, endpoint, "")
		}
		if err := json.Unmarshal(cached.body, out); err != nil {
			c.pages.remove(c.pages.pages[key])
			resp.Body.Close()
			return c.do(method, u, in, out, endpoint, "")
		}
		if link == "" {
			next = cached.next
		} else {
			cached.next = next
			c.pages.save(cached)
		}
		return next, nil
	}
	const maxResponse = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, redact(u), err)
	}
	if len(body) > maxResponse {
		return "", fmt.Errorf("%s %s: response exceeds %d bytes", method, redact(u), maxResponse)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		status := &StatusError{Method: method, Path: redact(u), Status: resp.Status, Message: e.Message, Code: resp.StatusCode,
			RateLimited: resp.StatusCode == 429 || resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(e.Message), "secondary rate limit")}
		if c.backoff != nil && IsRateLimited(status) {
			c.backoff.record(u, time.Now(), resp.Header)
		}
		if c.pages != nil {
			switch {
			case status.Code == http.StatusUnauthorized:
				c.pages.clear()
			case IsAccessDenied(status):
				c.pages.invalidateRepoURL(u)
				fallthrough
			case IsNotFound(status):
				c.pages.remove(c.pages.pages[key])
				c.pages.remove(c.pages.pages[pageKey{root: u, url: u}])
			}
		}
		return "", status
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return "", fmt.Errorf("%s %s: %w", method, redact(u), err)
		}
	}
	if root != "" && out != nil {
		if endpoint == "repo_inventory" && next == "" {
			var repos []json.RawMessage
			if json.Unmarshal(body, &repos) == nil && len(repos) == 100 {
				etag = ""
			}
		}
		pool := activePool
		if endpoint == "runs_created" || endpoint == "jobs_all" {
			pool = historyPool
		}
		c.pages.save(cachedPage{pool: pool, key: key, finalURL: finalURL, etag: etag, next: next, body: body})
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

// Repo is one of an owner's repositories.
type Repo struct {
	Name    string
	Private bool
}

// Repos lists owner's non-archived repositories. A user owner must be the
// token's user, since only they can list their private repositories.
func (c *Client) Repos(owner string, org bool) ([]Repo, error) {
	path := "/user/repos?affiliation=owner&per_page=100"
	if org {
		path = "/orgs/" + url.PathEscape(owner) + "/repos?type=all&per_page=100"
	}
	var repos []Repo
	err := c.ConditionalPaginate(path, func(body []byte) error {
		var page []struct {
			FullName string `json:"full_name"`
			Archived bool   `json:"archived"`
			Private  bool   `json:"private"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return err
		}
		for _, r := range page {
			if !r.Archived && strings.EqualFold(r.Owner.Login, owner) {
				repos = append(repos, Repo{Name: r.FullName, Private: r.Private})
			}
		}
		return nil
	})
	if err == nil && c.pages != nil {
		c.pages.setInventory(c.BaseURL+path+"#"+strings.ToLower(owner), repos)
	}
	return repos, err
}
