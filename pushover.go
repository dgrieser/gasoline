package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const pushoverMessagesURL = "https://api.pushover.net/1/messages.json"

// pushoverMessageLimit is the API's maximum message length.
const pushoverMessageLimit = 1024

type pushoverMessage struct {
	Token   string
	UserKey string
	Title   string
	Message string
}

// sendPushover posts one notification to the Pushover message API. A non-2xx
// response or an API status other than 1 is an error carrying the API's
// error strings when present.
func sendPushover(ctx context.Context, apiURL string, msg pushoverMessage) error {
	message := msg.Message
	if len(message) > pushoverMessageLimit {
		message = message[:pushoverMessageLimit]
	}
	form := url.Values{
		"token":   {msg.Token},
		"user":    {msg.UserKey},
		"message": {message},
	}
	if msg.Title != "" {
		form.Set("title", msg.Title)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var apiResp struct {
		Status int      `json:"status"`
		Errors []string `json:"errors"`
	}
	parseErr := json.Unmarshal(body, &apiResp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parseErr != nil || apiResp.Status != 1 {
		if parseErr == nil && len(apiResp.Errors) > 0 {
			return fmt.Errorf("pushover: %s (HTTP %d)", strings.Join(apiResp.Errors, "; "), resp.StatusCode)
		}
		return fmt.Errorf("pushover: unexpected response (HTTP %d)", resp.StatusCode)
	}
	return nil
}
