package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Verifier checks the token a visitor's browser got from a challenge, to
// tell people from scripts before a scan is started.
type Verifier interface {
	// Verify reports whether token is a valid, unused token. visitor is the
	// visitor's address, if known. A non-nil error means the check could
	// not be made, which says nothing about the visitor.
	Verify(ctx context.Context, token, visitor string) (bool, error)
}

// Turnstile verifies Cloudflare Turnstile tokens.
//
// The site shows Turnstile's widget, which gives the browser a short-lived,
// single-use token; the token is sent with the request to start a scan and
// checked here against Cloudflare with the site's secret key.
type Turnstile struct {
	// Secret is the widget's secret key.
	Secret string
	// URL is the verification endpoint; overridable for tests.
	URL string
	// Client makes the request (nil: a client with a 5 s timeout).
	Client *http.Client
}

const turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

var turnstileClient = &http.Client{Timeout: 5 * time.Second}

// maxTokenLength is Cloudflare's stated maximum length of a token.
const maxTokenLength = 2048

func (t Turnstile) Verify(ctx context.Context, token, visitor string) (bool, error) {
	if token == "" || len(token) > maxTokenLength {
		return false, nil
	}
	form := url.Values{"secret": {t.Secret}, "response": {token}}
	if visitor != "" && visitor != "unknown" {
		form.Set("remoteip", visitor)
	}
	endpoint := t.URL
	if endpoint == "" {
		endpoint = turnstileVerifyURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := t.Client
	if client == nil {
		client = turnstileClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, errors.New("the verification service could not be reached")
	}
	defer resp.Body.Close()
	var out struct {
		Success bool     `json:"success"`
		Errors  []string `json:"error-codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return false, fmt.Errorf("unexpected answer from the verification service (HTTP %d)", resp.StatusCode)
	}
	if !out.Success {
		for _, code := range out.Errors {
			// These mean the server is set up wrongly, not that the visitor
			// failed: say so instead of blaming the visitor.
			if code == "missing-input-secret" || code == "invalid-input-secret" {
				return false, errors.New("the verification secret key is not accepted")
			}
		}
	}
	return out.Success, nil
}
