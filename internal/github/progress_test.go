package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestProgressBeforeRequestWithoutQueryOrCredentials(t *testing.T) {
	var started atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !started.Load() {
			t.Error("request reached server before progress")
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	gh := New("fixture-token-not-for-progress")
	gh.BaseURL = srv.URL
	gh.Progress = func(path string) {
		if path != "/user" {
			t.Errorf("progress path includes extra data: %q", path)
		}
		started.Store(true)
	}
	if err := gh.Get("/user?marker=not-for-progress", nil); err != nil {
		t.Fatal(err)
	}
	if !started.Load() {
		t.Fatal("no progress callback")
	}
}
