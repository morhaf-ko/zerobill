package github

import (
	"net/http"
	"strconv"
	"time"
)

type backoff struct {
	until     time.Time
	secondary time.Duration
	retryURL  string
}

// EnableBackoff shares rate-limit state across the controller's checks.
func (c *Client) EnableBackoff() {
	c.backoff = &backoff{}
}

func (b *backoff) record(url string, now time.Time, h http.Header) {
	if b.retryURL != url {
		b.secondary = 0
		b.retryURL = url
	}
	var until time.Time
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			until = time.Unix(reset, 0)
		}
	}
	if seconds, err := strconv.ParseInt(h.Get("Retry-After"), 10, 32); err == nil && seconds > 0 {
		if after := now.Add(time.Duration(seconds) * time.Second); after.After(until) {
			until = after
		}
	} else if after, err := http.ParseTime(h.Get("Retry-After")); err == nil && after.After(until) {
		until = after
	}
	if !until.After(now) {
		b.secondary = min(time.Hour, max(time.Minute, 2*b.secondary))
		until = now.Add(b.secondary)
	}
	b.until = until
}

func (b *backoff) succeeded(url string) {
	if b.retryURL == url {
		b.secondary = 0
		b.retryURL = ""
	}
}
