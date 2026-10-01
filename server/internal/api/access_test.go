package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"websitemapper/internal/api"
	"websitemapper/internal/discovery"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
	"websitemapper/internal/store/storetest"
)

// serverWith runs the API with the given access rules. Scans complete
// almost at once and, like in production, recent ones are reused.
func serverWith(t *testing.T, access api.Access) *httptest.Server {
	t.Helper()
	svc := scan.NewService(storetest.New(t, store.Options{MaxScans: 50}),
		[]scan.Stage{{ID: "crawl", Label: "Crawling", Engines: []discovery.Engine{oneFinding{}}}},
		scan.Options{QueueSize: 50, MaxRunning: 4, ProgressInterval: 10 * time.Millisecond, ReuseTTL: time.Hour}, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	t.Cleanup(func() { cancel(); svc.Wait() })
	srv := httptest.NewServer(api.NewHandler(svc, quiet, access))
	t.Cleanup(srv.Close)
	return srv
}

// send makes a request with headers and returns the status, the decoded
// body and the response headers.
func send(t *testing.T, method, url, body string, headers map[string]string) (int, map[string]any, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out, resp.Header
}

func TestClosedAPIAnswersOnlyTheWebClient(t *testing.T) {
	srv := serverWith(t, api.Access{ProxySecret: "s3cret"})
	viaClient := map[string]string{"X-Scanner-Secret": "s3cret"}
	create := `{"target":"example.com"}`

	// Without the secret, or with a wrong one, nothing but "it is up".
	for _, h := range []map[string]string{nil, {"X-Scanner-Secret": "guess"}, {"X-Scanner-Secret": ""}} {
		if code, body, _ := send(t, "POST", srv.URL+"/api/scans", create, h); code != http.StatusUnauthorized || body["error"] == "" {
			t.Errorf("create with %v = %d %v", h, code, body)
		}
		if code, _, _ := send(t, "GET", srv.URL+"/api/scans/abc/summary", "", h); code != http.StatusUnauthorized {
			t.Errorf("summary with %v = %d", h, code)
		}
		code, body, _ := send(t, "GET", srv.URL+"/api/health", "", h)
		if code != 200 || body["status"] != "ok" || len(body) != 1 {
			t.Errorf("health with %v = %d %v, want only the status", h, code, body)
		}
	}

	// With it, everything works as on an open API.
	code, body, _ := send(t, "POST", srv.URL+"/api/scans", create, viaClient)
	if code != http.StatusAccepted || body["id"] == "" {
		t.Fatalf("create through the web client = %d %v", code, body)
	}
	if code, body, _ := send(t, "GET", srv.URL+"/api/scans/"+body["id"].(string), "", viaClient); code != 200 || body["domain"] != "example.com" {
		t.Errorf("status through the web client = %d %v", code, body)
	}
	if code, body, _ := send(t, "GET", srv.URL+"/api/health", "", viaClient); code != 200 || body["scheduler"] == nil || body["process"] == nil {
		t.Errorf("health through the web client = %d %v", code, body)
	}
}

func TestOpenAPIIgnoresTheSecretHeader(t *testing.T) {
	srv := serverWith(t, api.Access{})
	if code, body, _ := send(t, "GET", srv.URL+"/api/health", "", nil); code != 200 || body["scheduler"] == nil {
		t.Errorf("health = %d %v", code, body)
	}
	if code, _, _ := send(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`, map[string]string{"X-Scanner-Secret": "anything"}); code != http.StatusAccepted {
		t.Errorf("create = %d", code)
	}
}

func TestStartLimitPerVisitor(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	srv := serverWith(t, api.Access{
		ProxySecret: "s3cret", StartLimit: 2, StartWindow: 10 * time.Minute,
		Now: func() time.Time { return time.Unix(0, now.Load()) },
	})
	start := func(visitor, target string, fresh bool) (int, map[string]any, http.Header) {
		body := `{"target":"` + target + `"}`
		if fresh {
			body = `{"target":"` + target + `","fresh":true}`
		}
		return send(t, "POST", srv.URL+"/api/scans", body, map[string]string{"X-Scanner-Secret": "s3cret", "X-Scanner-Client": visitor})
	}
	waitDone := func(id string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			_, body, _ := send(t, "GET", srv.URL+"/api/scans/"+id, "", map[string]string{"X-Scanner-Secret": "s3cret"})
			if body["status"] == "completed" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("scan %s did not complete", id)
	}

	// Two scans are allowed...
	_, first, _ := start("203.0.113.7", "a.example.com", false)
	waitDone(first["id"].(string))
	now.Add(int64(4 * time.Minute))
	if code, _, _ := start("203.0.113.7", "b.example.com", false); code != http.StatusAccepted {
		t.Fatalf("second scan = %d", code)
	}
	// ...getting a finished scan back costs nothing...
	if code, body, _ := start("203.0.113.7", "a.example.com", false); code != 200 || body["reused"] != true {
		t.Fatalf("reused scan = %d %v", code, body)
	}
	// ...and the third new scan is refused, with when to come back.
	var (
		code int
		body map[string]any
		hdr  http.Header
	)
	code, body, hdr = start("203.0.113.7", "c.example.com", false)
	if code != http.StatusTooManyRequests || !strings.Contains(body["error"].(string), "6 minutes") || hdr.Get("Retry-After") != "361" {
		t.Errorf("third scan = %d %v, Retry-After %q", code, body, hdr.Get("Retry-After"))
	}
	// A refused request must not have started anything, also not a rescan.
	if code, _, _ := start("203.0.113.7", "a.example.com", true); code != http.StatusTooManyRequests {
		t.Errorf("fresh rescan over the limit = %d", code)
	}

	// Another visitor has their own allowance.
	code, other, _ := start("198.51.100.9", "c.example.com", false)
	if code != http.StatusAccepted {
		t.Fatalf("another visitor = %d", code)
	}
	waitDone(other["id"].(string)) // so that a later scan of it is a new one
	// An invalid request does not use up the allowance.
	for i := 0; i < 3; i++ {
		if code, _, _ := start("192.0.2.44", "not a domain", false); code != http.StatusBadRequest {
			t.Errorf("invalid target = %d", code)
		}
	}
	if code, _, _ := start("192.0.2.44", "d.example.com", false); code != http.StatusAccepted {
		t.Errorf("a visitor with only invalid requests so far = %d", code)
	}

	// When the first start leaves the window, one more scan is allowed.
	now.Add(int64(6*time.Minute + time.Second))
	if code, _, _ := start("203.0.113.7", "c.example.com", true); code != http.StatusAccepted {
		t.Errorf("after the window moved on = %d", code)
	}
	if code, _, _ := start("203.0.113.7", "e.example.com", false); code != http.StatusTooManyRequests {
		t.Errorf("the limit still holds = %d", code)
	}
}

// On an open API, visitors behind a reverse proxy are told apart by the
// address it forwards; the web client's header is not believed there.
func TestStartLimitOnAnOpenAPI(t *testing.T) {
	srv := serverWith(t, api.Access{StartLimit: 1, StartWindow: time.Hour})
	start := func(target string, h map[string]string) int {
		code, _, _ := send(t, "POST", srv.URL+"/api/scans", `{"target":"`+target+`"}`, h)
		return code
	}
	if code := start("a.example.com", map[string]string{"X-Forwarded-For": "203.0.113.7, 10.0.0.1"}); code != http.StatusAccepted {
		t.Fatalf("first visitor = %d", code)
	}
	if code := start("b.example.com", map[string]string{"X-Forwarded-For": "203.0.113.7"}); code != http.StatusTooManyRequests {
		t.Errorf("same visitor again = %d", code)
	}
	if code := start("b.example.com", map[string]string{"X-Forwarded-For": "198.51.100.9"}); code != http.StatusAccepted {
		t.Errorf("second visitor = %d", code)
	}
	// Claiming to be someone else through the web client's header does
	// nothing without the secret.
	if code := start("c.example.com", map[string]string{"X-Forwarded-For": "203.0.113.7", "X-Scanner-Client": "192.0.2.1"}); code != http.StatusTooManyRequests {
		t.Errorf("spoofed client header = %d", code)
	}
}
