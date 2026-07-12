package kernel

// YesCaptcha Turnstile solver client.
//
// YesCaptcha is anti-captcha-API-compatible. For Cloudflare Turnstile
// we use TurnstileTaskProxyless: YesCaptcha runs the challenge on
// their side, no proxy plumbing needed here.
//
// Flow:
//   POST /createTask       {clientKey, task:{type, websiteURL, websiteKey}}
//       → {errorId:0, taskId:<int>}
//   POST /getTaskResult    {clientKey, taskId}
//       → {status:"processing"}  (poll)
//       → {status:"ready", solution:{token:"..."}}
//
// Typical solve time: 15-40s. We cap at 90s (docs default) — beyond
// that either the account is out of credit or the challenge is
// broken, and the caller should fail the login.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Defaults for the solver. yescaptchaBaseURL is a var so tests can
// point it at an httptest.Server without touching env.
const (
	yescaptchaDefaultTimeout = 90 * time.Second
	yescaptchaPollInterval   = 3 * time.Second
)

// yescaptchaSolver drives a single Turnstile solve against YesCaptcha.
// One instance per solve; not concurrent-safe.
type yescaptchaSolver struct {
	APIKey     string
	BaseURL    string        // default "https://api.yescaptcha.com"
	HTTPClient *http.Client  // default with sensible timeouts
	Timeout    time.Duration // per-solve deadline (default yescaptchaDefaultTimeout)
	PollEvery  time.Duration // between /getTaskResult polls (default yescaptchaPollInterval)
}

// solve runs createTask, polls getTaskResult, and returns the raw
// Turnstile token on success.
func (s *yescaptchaSolver) solve(ctx context.Context, websiteURL, websiteKey string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(s.BaseURL), "/")
	if base == "" {
		base = "https://api.yescaptcha.com"
	}
	key := strings.TrimSpace(s.APIKey)
	if key == "" {
		return "", fmt.Errorf("yescaptcha api key is empty")
	}
	client := s.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = yescaptchaDefaultTimeout
	}
	pollEvery := s.PollEvery
	if pollEvery <= 0 {
		pollEvery = yescaptchaPollInterval
	}

	// 1. createTask
	createBody, _ := json.Marshal(map[string]any{
		"clientKey": key,
		"task": map[string]any{
			"type":       "TurnstileTaskProxyless",
			"websiteURL": websiteURL,
			"websiteKey": websiteKey,
		},
	})
	var created struct {
		ErrorID          int    `json:"errorId"`
		ErrorCode        string `json:"errorCode"`
		ErrorDescription string `json:"errorDescription"`
		TaskID           any    `json:"taskId"`
	}
	if err := postJSON(ctx, client, base+"/createTask", createBody, &created); err != nil {
		return "", fmt.Errorf("createTask: %w", err)
	}
	if created.ErrorID != 0 {
		return "", fmt.Errorf("createTask error %s: %s", created.ErrorCode, created.ErrorDescription)
	}
	if created.TaskID == nil {
		return "", fmt.Errorf("createTask returned no taskId")
	}

	// 2. poll getTaskResult
	deadline := time.Now().Add(timeout)
	lastStatus := "processing"
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("solve timeout after %s (last status: %s)", timeout, lastStatus)
		}

		// Sleep respecting ctx cancellation.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollEvery):
		}

		resultBody, _ := json.Marshal(map[string]any{
			"clientKey": key,
			"taskId":    created.TaskID,
		})
		var result struct {
			ErrorID          int    `json:"errorId"`
			ErrorCode        string `json:"errorCode"`
			ErrorDescription string `json:"errorDescription"`
			Status           string `json:"status"`
			Solution         struct {
				Token              string `json:"token"`
				GRecaptchaResponse string `json:"gRecaptchaResponse"`
			} `json:"solution"`
		}
		if err := postJSON(ctx, client, base+"/getTaskResult", resultBody, &result); err != nil {
			return "", fmt.Errorf("getTaskResult: %w", err)
		}
		if result.ErrorID != 0 {
			return "", fmt.Errorf("getTaskResult error %s: %s", result.ErrorCode, result.ErrorDescription)
		}
		lastStatus = result.Status
		if result.Status == "ready" {
			tok := result.Solution.Token
			if tok == "" {
				tok = result.Solution.GRecaptchaResponse
			}
			if tok == "" {
				return "", fmt.Errorf("getTaskResult ready but no token")
			}
			return tok, nil
		}
	}
}

// postJSON is a tiny helper: POST payload as JSON, decode response
// body as JSON into out.
func postJSON(ctx context.Context, client *http.Client, url string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			_ = cerr
		}
	}()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if len(raw) == 0 {
		return fmt.Errorf("empty response body")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode json: %w (body: %s)", err, truncate(string(raw), 200))
	}
	return nil
}

// truncate returns s truncated to n runes with an ellipsis suffix.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
