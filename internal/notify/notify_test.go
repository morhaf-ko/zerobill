package notify

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBody(t *testing.T) {
	for target, want := range map[string]string{
		"https://ntfy.sh/home-ci":                     "hi",
		"https://discord.com/api/webhooks/1/secret":   `{"content":"hi"}`,
		"https://hooks.slack.com/services/T/B/secret": `{"text":"hi"}`,
	} {
		if _, got := Body(target, "hi"); string(got) != want {
			t.Errorf("%s: got %s, want %s", target, got, want)
		}
	}
}

func TestSend(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = r.Header.Get("Content-Type") + " " + string(b)
	}))
	defer srv.Close()
	if err := Send(srv.URL+"/topic", "CI_RUNNER now ubuntu-latest"); err != nil || got != "text/plain; charset=utf-8 CI_RUNNER now ubuntu-latest" {
		t.Fatalf("got %q %v", got, err)
	}
	if err := Send("http://127.0.0.1:1/secret-path", "x"); err == nil || strings.Contains(err.Error(), "secret-path") {
		t.Fatalf("want a redacted error, got %v", err)
	}
}
