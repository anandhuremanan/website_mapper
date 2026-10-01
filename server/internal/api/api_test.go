package api_test

import (
	"compress/gzip"
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
	"websitemapper/internal/store/storetest"
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
	svc := scan.NewService(storetest.New(t, store.Options{MaxScans: 10}), []scan.Stage{{ID: "crawl", Label: "Crawling", Engines: []discovery.Engine{oneFinding{}}}},
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

func TestResponsesAreGzipped(t *testing.T) {
	srv := newServer(t, false)
	req, _ := http.NewRequest("GET", srv.URL+"/api/health", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	// A Transport with compression disabled shows the raw encoding.
	resp, err := (&http.Transport{DisableCompression: true}).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q", resp.Header.Get("Content-Encoding"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(zr).Decode(&body); err != nil || body["status"] != "ok" || body["process"] == nil {
		t.Errorf("decoded %v, %v", body, err)
	}
	// Clients that do not ask for gzip get plain JSON.
	code, plain := do(t, "GET", srv.URL+"/api/health", "")
	if code != 200 || plain["status"] != "ok" {
		t.Errorf("plain = %d %v", code, plain)
	}
}

func TestCreateScanMode(t *testing.T) {
	srv := newServer(t, false)
	code, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","mode":"passive"}`)
	if code != http.StatusAccepted || body["mode"] != "passive" {
		t.Errorf("passive = %d %v", code, body)
	}
	if code, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`); body["mode"] != "light" {
		t.Errorf("default = %d %v", code, body["mode"])
	}
	if code, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","mode":"deep"}`); code != http.StatusBadRequest {
		t.Errorf("invalid mode = %d %v", code, body)
	}
}

// finishedScan runs a scan to completion and returns its ID.
func finishedScan(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	id := body["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body = do(t, "GET", srv.URL+"/api/scans/"+id, "")
		if body["status"] == "completed" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan did not complete: %v", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPagedResults(t *testing.T) {
	srv := newServer(t, true)
	base := srv.URL + "/api/scans/" + finishedScan(t, srv)

	// The summary has everything but the hosts and URLs.
	code, body := do(t, "GET", base+"/summary", "")
	if code != 200 || body["status"] != "completed" || body["counts"].(map[string]any)["urls"] != float64(1) {
		t.Fatalf("summary = %d %v", code, body)
	}
	if _, has := body["hosts"]; has {
		t.Errorf("summary must not list hosts: %v", body["hosts"])
	}
	if body["technologies"] == nil || body["limits"] == nil {
		t.Errorf("summary = %v", body)
	}

	code, body = do(t, "GET", base+"/hosts?limit=10", "")
	hosts, _ := body["hosts"].([]any)
	if code != 200 || len(hosts) != 1 {
		t.Fatalf("hosts = %d %v", code, body)
	}
	host := hosts[0].(map[string]any)
	if _, has := host["urls"]; has || host["hostname"] != "example.com" || host["counts"].(map[string]any)["urls"] != float64(1) {
		t.Errorf("host = %v", host)
	}
	if _, has := body["next"]; has {
		t.Errorf("a last page must not have a next cursor: %v", body["next"])
	}

	code, body = do(t, "GET", base+"/urls?host=example.com&type=page,unknown", "")
	urls, _ := body["urls"].([]any)
	if code != 200 || len(urls) != 1 || urls[0].(map[string]any)["title"] != "Home" {
		t.Fatalf("urls = %d %v", code, body)
	}
	if code, body = do(t, "GET", base+"/urls?type=api", ""); code != 200 || len(body["urls"].([]any)) != 0 {
		t.Errorf("api urls = %d %v", code, body)
	}

	code, body = do(t, "GET", base+"/tree?host=example.com", "")
	if code != 200 || body["path"] != "/" || body["total"] != float64(1) || len(body["urls"].([]any)) != 1 || len(body["children"].([]any)) != 0 {
		t.Errorf("tree = %d %v", code, body)
	}

	for path, want := range map[string]int{
		"/urls?type=pages":              400,
		"/urls?limit=0":                 400,
		"/urls?after=nonsense":          400,
		"/hosts?limit=x":                400,
		"/tree":                         400,
		"/tree?host=example.com&path=x": 400,
		"/tree?host=other.example.com":  404,
	} {
		if code, body := do(t, "GET", base+path, ""); code != want || body["error"] == "" {
			t.Errorf("GET %s = %d %v, want %d", path, code, body, want)
		}
	}
}

func TestExport(t *testing.T) {
	srv := newServer(t, true)
	resp, err := http.Get(srv.URL + "/api/scans/" + finishedScan(t, srv) + "/export")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "example.com-urls.csv") {
		t.Errorf("export = %d %v", resp.StatusCode, resp.Header)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "url,host,type,") ||
		!strings.HasPrefix(lines[1], "https://example.com/,example.com,page,,verified,200,text/html,Home,target,") {
		t.Errorf("export body =\n%s", b)
	}
}

func TestPagedResultsBeforeFinish(t *testing.T) {
	srv := newServer(t, false) // no workers: the scan stays queued
	_, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com"}`)
	base := srv.URL + "/api/scans/" + body["id"].(string)
	for _, path := range []string{"/summary", "/hosts", "/urls", "/tree?host=example.com", "/export"} {
		if code, body := do(t, "GET", base+path, ""); code != http.StatusConflict || body["status"] != "queued" {
			t.Errorf("GET %s = %d %v, want 409", path, code, body)
		}
	}
	if code, _ := do(t, "GET", srv.URL+"/api/scans/nope/summary", ""); code != http.StatusNotFound {
		t.Errorf("unknown scan = %d", code)
	}
}
