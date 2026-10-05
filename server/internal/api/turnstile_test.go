package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"websitemapper/internal/api"
)

// siteverify imitates Cloudflare's verification endpoint: the token "good"
// is valid once for the secret "s3cret".
func siteverify(t *testing.T) (*httptest.Server, *[]string) {
	var (
		mu   sync.Mutex
		used = map[string]bool{}
		ips  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		ips = append(ips, r.PostForm.Get("remoteip"))
		out := map[string]any{"success": false}
		switch token := r.PostForm.Get("response"); {
		case r.PostForm.Get("secret") != "s3cret":
			out["error-codes"] = []string{"invalid-input-secret"}
		case token == "good" && !used[token]:
			used[token] = true
			out["success"] = true
		case token == "good":
			out["error-codes"] = []string{"timeout-or-duplicate"}
		default:
			out["error-codes"] = []string{"invalid-input-response"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &ips
}

func TestTurnstileVerify(t *testing.T) {
	srv, ips := siteverify(t)
	ts := api.Turnstile{Secret: "s3cret", URL: srv.URL}
	ctx := context.Background()

	if ok, err := ts.Verify(ctx, "good", "203.0.113.7"); !ok || err != nil {
		t.Errorf("valid token = %v, %v", ok, err)
	}
	if (*ips)[0] != "203.0.113.7" {
		t.Errorf("visitor address sent = %q", (*ips)[0])
	}
	// A token works once.
	if ok, err := ts.Verify(ctx, "good", "203.0.113.7"); ok || err != nil {
		t.Errorf("reused token = %v, %v", ok, err)
	}
	if ok, err := ts.Verify(ctx, "made-up", ""); ok || err != nil {
		t.Errorf("invalid token = %v, %v", ok, err)
	}
	// Nothing is asked for a missing or absurdly long token.
	before := len(*ips)
	for _, token := range []string{"", strings.Repeat("x", 3000)} {
		if ok, err := ts.Verify(ctx, token, ""); ok || err != nil {
			t.Errorf("token of length %d = %v, %v", len(token), ok, err)
		}
	}
	if len(*ips) != before {
		t.Error("the verification service was asked about an unusable token")
	}

	// A wrong secret key or an unreachable service is an error, not a
	// verdict on the visitor.
	if ok, err := (api.Turnstile{Secret: "wrong", URL: srv.URL}).Verify(ctx, "good", ""); ok || err == nil {
		t.Errorf("wrong secret = %v, %v", ok, err)
	}
	srv.Close()
	if ok, err := ts.Verify(ctx, "good", ""); ok || err == nil {
		t.Errorf("unreachable service = %v, %v", ok, err)
	}
}

// fakeVerifier accepts the token "human" and fails for the token "outage".
type fakeVerifier struct {
	mu       sync.Mutex
	visitors []string
}

func (f *fakeVerifier) Verify(_ context.Context, token, visitor string) (bool, error) {
	f.mu.Lock()
	f.visitors = append(f.visitors, visitor)
	f.mu.Unlock()
	if token == "outage" {
		return false, errors.New("the verification service could not be reached")
	}
	return token == "human", nil
}

func TestScansNeedAVerifiedVisitor(t *testing.T) {
	v := &fakeVerifier{}
	srv := serverWith(t, api.Access{ProxySecret: "s3cret", Verifier: v})
	h := map[string]string{"X-Scanner-Secret": "s3cret", "X-Scanner-Client": "203.0.113.7"}
	create := func(body string) (int, map[string]any) {
		code, out, _ := send(t, "POST", srv.URL+"/api/scans", body, h)
		return code, out
	}

	// No token, or a bad one: refused, and nothing is started.
	for _, body := range []string{`{"target":"example.com"}`, `{"target":"example.com","verificationToken":"bot"}`} {
		if code, out := create(body); code != http.StatusForbidden || !strings.Contains(out["error"].(string), "verification") {
			t.Errorf("%s = %d %v", body, code, out)
		}
	}
	if _, health, _ := send(t, "GET", srv.URL+"/api/health", "", h); health["scheduler"].(map[string]any)["running"] != float64(0) {
		t.Errorf("a refused request started a scan: %v", health["scheduler"])
	}

	// The verification service being down is not the visitor's fault.
	if code, out := create(`{"target":"example.com","verificationToken":"outage"}`); code != http.StatusServiceUnavailable || out["error"] == "" {
		t.Errorf("outage = %d %v", code, out)
	}

	// A verified visitor starts a scan; the visitor's address was passed on.
	code, out := create(`{"target":"example.com","verificationToken":"human"}`)
	if code != http.StatusAccepted || out["id"] == "" {
		t.Fatalf("verified = %d %v", code, out)
	}
	if got := v.visitors[len(v.visitors)-1]; got != "203.0.113.7" {
		t.Errorf("visitor passed to the verifier = %q", got)
	}

	// Reading results never needs a token.
	if code, _, _ := send(t, "GET", srv.URL+"/api/scans/"+out["id"].(string), "", h); code != 200 {
		t.Errorf("status = %d", code)
	}
}
