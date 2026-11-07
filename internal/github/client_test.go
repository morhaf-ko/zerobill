package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPaginateRefusesForeignNextLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://elsewhere.example/page2>; rel="next"`)
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()
	c := New("secret")
	c.BaseURL = srv.URL
	err := c.Paginate("/x", func([]byte) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "leaves") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

func TestErrorsOmitToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("headers %v", r.Header)
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	c := New("secret")
	c.BaseURL = srv.URL
	var out json.RawMessage
	err := c.Get("/users/x", &out)
	if err == nil || !strings.Contains(err.Error(), "/users/x: 404 Not Found: Not Found") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unexpected error %v", err)
	}
}
