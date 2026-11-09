package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccessErrorsExcludeRateLimits(t *testing.T) {
	for _, tt := range []struct {
		code             int
		remaining, retry string
		want             bool
	}{
		{401, "", "", true}, {401, "0", "", true}, {403, "", "", true}, {403, "0", "", false},
		{403, "", "60", false}, {429, "", "", false}, {404, "", "", false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tt.remaining != "" {
				w.Header().Set("X-RateLimit-Remaining", tt.remaining)
			}
			if tt.retry != "" {
				w.Header().Set("Retry-After", tt.retry)
			}
			w.WriteHeader(tt.code)
			fmt.Fprint(w, `{"message":"request failed"}`)
		}))
		gh := New("fixture-token")
		gh.BaseURL = srv.URL
		err := gh.Get("/user", nil)
		srv.Close()
		if got := IsAccessDenied(fmt.Errorf("wrapped: %w", err)); got != tt.want {
			t.Fatalf("code %d, remaining %q, retry %q: got %v, want %v", tt.code, tt.remaining, tt.retry, got, tt.want)
		}
	}
}

func TestSecondaryLimitWithoutRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit."}`)
	}))
	defer srv.Close()
	gh := New("fixture-token")
	gh.BaseURL = srv.URL
	err := gh.Get("/user", nil)
	if IsAccessDenied(err) || !IsRateLimited(err) {
		t.Fatalf("wrong classification: %v", err)
	}
}
