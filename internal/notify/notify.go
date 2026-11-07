// Package notify sends a one-line message to ntfy, Discord, Slack or any URL
// that accepts a plain-text POST.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Second}

// Send posts text to target. Discord and Slack webhooks get their JSON shape;
// anything else, such as an ntfy topic, gets plain text. Errors never contain
// the URL, whose path is often the webhook's secret.
func Send(target, text string) error {
	contentType, b := body(target, text)
	resp, err := client.Post(target, contentType, bytes.NewReader(b))
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("notify: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: %s", resp.Status)
	}
	return nil
}

func body(target, text string) (string, []byte) {
	field := ""
	if u, err := url.Parse(target); err == nil {
		switch host := strings.ToLower(u.Hostname()); {
		case host == "discord.com" || host == "discordapp.com" || strings.HasSuffix(host, ".discord.com"):
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
