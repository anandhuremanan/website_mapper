package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"websitemapper/internal/api"
	"websitemapper/internal/discovery"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
)

type oneFinding struct{}

func (oneFinding) Name() string { return "html" }
func (oneFinding) Discover(_ context.Context, in discovery.Input, emit discovery.Emit) error {
	emit(discovery.Finding{URL: in.Target.StartURL, Source: discovery.SourceTarget, Hint: discovery.HintEntry,
		Response: &discovery.Response{Status: 200, ContentType: "text/html", Title: "Home"}})
	return nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(t *testing.T, workers bool) *httptest.Server {
	t.Helper()
	svc := scan.NewService(store.NewMemory(store.Limits{MaxScans: 10}), []scan.Stage{{ID: "crawl", Label: "Crawling", Engines: []discovery.Engine{oneFinding{}}}},
		scan.Options{QueueSize: 5, ProgressInterval: 10 * time.Millisecond}, quiet)
	if workers {
		ctx, cancel := context.WithCancel(context.Background())
		svc.Start(ctx)
		t.Cleanup(func() { cancel(); svc.Wait() })
	}
	srv := httptest.NewServer(api.NewHandler(svc, quiet))
	t.Cleanup(srv.Close)
	return srv
}

func do(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestHealth(t *testing.T) {
	srv := newServer(t, false)
	code, body := do(t, "GET", srv.URL+"/api/health", "")
	if code != 200 || body["status"] != "ok" {
		t.Errorf("health = %d %v", code, body)
	}
	sched, _ := body["scheduler"].(map[string]any)
	if sched == nil || sched["maxRunning"] == nil || sched["queued"] == nil {
		t.Errorf("health scheduler = %v", body["scheduler"])
	}
}

func TestCancelScan(t *testing.T) {
	// No workers: scans stay queued, so cancellation is deterministic.
	srv := newServer(t, false)
	_, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	id := body["id"].(string)
	if body["queuePosition"] != float64(1) {
		t.Errorf("queuePosition = %v", body["queuePosition"])
	}

	code, body := do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", "")
	if code != http.StatusAccepted || body["status"] != "cancelled" || body["stopReason"] != "cancelled" {
		t.Fatalf("cancel = %d %v", code, body)
	}
	if code, body = do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", ""); code != http.StatusConflict || body["status"] != "cancelled" {
		t.Errorf("second cancel = %d %v", code, body)
	}
	if code, _ = do(t, "POST", srv.URL+"/api/scans/missing/cancel", ""); code != http.StatusNotFound {
		t.Errorf("cancel missing = %d", code)
	}
	// A scan cancelled before running has no results.
	if code, body = do(t, "GET", srv.URL+"/api/scans/"+id+"/results", ""); code != http.StatusConflict || body["status"] != "cancelled" {
		t.Errorf("results = %d %v", code, body)
	}
}

func TestScanFlow(t *testing.T) {
	srv := newServer(t, true)

	code, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	// Capacity is free, so the scan starts at once.
	if code != http.StatusAccepted || body["status"] != "running" || body["id"] == "" {
		t.Fatalf("create = %d %v", code, body)
	}
	id := body["id"].(string)

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body = do(t, "GET", srv.URL+"/api/scans/"+id, "")
		if code != 200 {
			t.Fatalf("get = %d %v", code, body)
		}
		if body["status"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not complete: %v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	code, body = do(t, "GET", srv.URL+"/api/scans/"+id+"/results", "")
	if code != 200 {
		t.Fatalf("results = %d %v", code, body)
	}
	hosts := body["hosts"].([]any)
	host := hosts[0].(map[string]any)
	urls := host["urls"].([]any)
	if len(hosts) != 1 || host["hostname"] != "example.com" || len(urls) != 1 || urls[0].(map[string]any)["title"] != "Home" {
		t.Errorf("hosts = %v", hosts)
	}
	if body["domain"].(map[string]any)["canonical"] != "example.com" {
		t.Errorf("domain = %v", body["domain"])
	}
}

func TestResultsBeforeFinish(t *testing.T) {
	srv := newServer(t, false) // no workers: the scan stays queued
	_, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	code, body := do(t, "GET", srv.URL+"/api/scans/"+body["id"].(string)+"/results", "")
	if code != http.StatusConflict || body["status"] != "queued" {
		t.Errorf("results = %d %v", code, body)
	}
}

func TestErrors(t *testing.T) {
	srv := newServer(t, false)
	tests := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/scans", `{"target":"localhost"}`, 400},
		{"POST", "/api/scans", `not json`, 400},
		{"POST", "/api/scans", `{"target":"example.com","extra":1}`, 400},
		{"GET", "/api/scans/missing", "", 404},
		{"GET", "/api/scans/missing/results", "", 404},
		{"GET", "/api/nope", "", 404},
		{"DELETE", "/api/scans/x", "", 404},
	}
	for _, tt := range tests {
		code, body := do(t, tt.method, srv.URL+tt.path, tt.body)
		if code != tt.want {
			t.Errorf("%s %s = %d, want %d (%v)", tt.method, tt.path, code, tt.want, body)
		}
		if body["error"] == "" {
			t.Errorf("%s %s: missing error message", tt.method, tt.path)
		}
	}
}

func TestCoalescedScansAndSubscriptionCancel(t *testing.T) {
	// No workers: scans stay queued, so the sequence is deterministic.
	srv := newServer(t, false)
	_, a := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	_, b := do(t, "POST", srv.URL+"/api/scans", `{"target":"https://example.com/"}`)
	if a["id"] != b["id"] || b["coalesced"] != true || a["subscriptionId"] == b["subscriptionId"] || b["subscriptionId"] == "" {
		t.Fatalf("a = %v\nb = %v", a, b)
	}
	id := a["id"].(string)
	if _, got := do(t, "GET", srv.URL+"/api/scans/"+id, ""); got["subscribers"] != float64(2) {
		t.Errorf("subscribers = %v", got["subscribers"])
	}
	// A shared scan needs a subscription to cancel.
	if code, body := do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", ""); code != http.StatusConflict {
		t.Errorf("cancel without subscription = %d %v", code, body)
	}
	code, body := do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", `{"subscriptionId":"`+b["subscriptionId"].(string)+`"}`)
	if code != http.StatusAccepted || body["detached"] != true || body["status"] != "queued" {
		t.Fatalf("B leaving = %d %v", code, body)
	}
	code, body = do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", `{"subscriptionId":"`+a["subscriptionId"].(string)+`"}`)
	if code != http.StatusAccepted || body["status"] != "cancelled" {
		t.Fatalf("A leaving = %d %v", code, body)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/scans/"+id+"/cancel", `{"bogus":1}`); code != http.StatusBadRequest && code != http.StatusConflict {
		t.Errorf("bad body = %d", code)
	}
	// Health reports coalescing and cache stats.
	_, health := do(t, "GET", srv.URL+"/api/health", "")
	if sched := health["scheduler"].(map[string]any); sched["coalescedRequests"] != float64(1) {
		t.Errorf("health = %v", sched)
	}
}
