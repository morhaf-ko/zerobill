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
		"https://ntfy.sh/home-ci":                       "hi",
		"https://discord.com/api/webhooks/1/secret":     `{"content":"hi"}`,
		"https://ptb.discord.com/api/webhooks/1/secret": `{"content":"hi"}`,
		"https://hooks.slack.com/services/T/B/secret":   `{"text":"hi"}`,
	} {
		if _, got := body(target, "hi"); string(got) != want {
			t.Errorf("%s: got %s, want %s", target, got, want)
		}
	}
}

func TestSend(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/topic":
			b, _ := io.ReadAll(r.Body)
			got = r.Header.Get("Content-Type") + " " + string(b)
		case "/redirect":
			http.Redirect(w, r, "http://127.0.0.1:1/token-in-redirect", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	if err := Send(srv.URL+"/topic", "CI_RUNNER now ubuntu-latest"); err != nil || got != "text/plain; charset=utf-8 CI_RUNNER now ubuntu-latest" {
		t.Fatalf("got %q %v", got, err)
	}
	for _, target := range []string{
		"http://127.0.0.1:1/secret-path",
		"http://user:secret-pass@127.0.0.1:1/secret-path",
		srv.URL + "/redirect",
		srv.URL + "/secret-path",
	} {
		err := Send(target, "x")
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
			t.Errorf("%s: want an error without the URL, got %v", target, err)
		}
	}
}
