// Package notify sends a one-line message to ntfy, Discord, Slack or any URL
// that accepts a plain-text POST.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// Send posts text to target. Discord and Slack webhooks get their JSON shape;
// anything else, such as an ntfy topic, gets plain text.
func Send(target, text string) error {
	contentType, body := Body(target, text)
	resp, err := client.Post(target, contentType, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: %w", redact(err, target))
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: %s", resp.Status)
	}
	return nil
}

// Body returns the content type and body Send posts to target.
func Body(target, text string) (string, []byte) {
	field := ""
	if u, err := url.Parse(target); err == nil {
		switch host := strings.ToLower(u.Hostname()); {
		case host == "discord.com" || host == "discordapp.com":
			field = "content"
		case host == "hooks.slack.com":
			field = "text"
		}
	}
	if field == "" {
		return "text/plain; charset=utf-8", []byte(text)
	}
	raw, _ := json.Marshal(map[string]string{field: text})
	return "application/json", raw
}

// redact keeps a webhook's secret path out of logs.
func redact(err error, target string) error {
	if u, perr := url.Parse(target); perr == nil {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), target, u.Scheme+"://"+u.Host))
	}
	return err
}
