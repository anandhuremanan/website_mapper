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
	svc := scan.NewService(store.NewMemory(10), []scan.Stage{{ID: "crawl", Label: "Crawling", Engines: []discovery.Engine{oneFinding{}}}},
		scan.Options{QueueSize: 5, ProgressInterval: 10 * time.Millisecond}, quiet)
	if workers {
		ctx, cancel := context.WithCancel(context.Background())
		svc.Start(ctx)
		t.Cleanup(func() { cancel(); svc.Wait() })
	}
	srv := httptest.NewServer(api.NewHandler(svc, api.BotInfo{Name: "WebsiteMapperBot", UserAgent: "WebsiteMapperBot/0.1 (+https://mapper.example/bot)", RespectsRobotsTxt: true}, quiet))
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
}

func TestScanFlow(t *testing.T) {
	srv := newServer(t, true)

	code, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","authorizationConfirmed":true}`)
	if code != http.StatusAccepted || body["status"] != "queued" || body["id"] == "" {
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
	_, body := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","authorizationConfirmed":true}`)
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
		{"POST", "/api/scans", `{"target":"localhost","authorizationConfirmed":true}`, 400},
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

func TestCreateRequiresAuthorizationConfirmation(t *testing.T) {
	srv := newServer(t, false)
	for _, body := range []string{
		`{"target":"example.com"}`,
		`{"target":"example.com","authorizationConfirmed":false}`,
	} {
		code, resp := do(t, "POST", srv.URL+"/api/scans", body)
		if code != http.StatusBadRequest || !strings.Contains(resp["error"].(string), "authorization not confirmed") {
			t.Errorf("%s: %d %v", body, code, resp)
		}
	}
	// A non-boolean value is rejected too.
	if code, _ := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","authorizationConfirmed":"yes"}`); code != http.StatusBadRequest {
		t.Errorf("string confirmation accepted: %d", code)
	}

	code, resp := do(t, "POST", srv.URL+"/api/scans", `{"target":"example.com","authorizationConfirmed":true}`)
	if code != http.StatusAccepted || resp["authorizationConfirmed"] != true {
		t.Errorf("confirmed: %d %v", code, resp)
	}
}

func TestBotInfo(t *testing.T) {
	srv := newServer(t, false)
	code, body := do(t, "GET", srv.URL+"/api/bot", "")
	if code != 200 || body["userAgent"] != "WebsiteMapperBot/0.1 (+https://mapper.example/bot)" || body["respectsRobotsTxt"] != true {
		t.Errorf("bot = %d %v", code, body)
	}
}
