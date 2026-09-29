package scan_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
	"websitemapper/internal/scan"
	"websitemapper/internal/store"
)

// fakeEngine emits fixed findings, then optionally fails or blocks. It
// records the hosts it saw, to check that later stages see earlier results.
type fakeEngine struct {
	name     string
	findings []discovery.Finding
	err      error
	block    bool

	mu        sync.Mutex
	sawHosts  [][]string
	callCount int
}

func (e *fakeEngine) Name() string { return e.name }
func (e *fakeEngine) Discover(ctx context.Context, in discovery.Input, emit discovery.Emit) error {
	var hosts []string
	for _, h := range in.State.Hosts() {
		hosts = append(hosts, h.Name)
	}
	e.mu.Lock()
	e.sawHosts = append(e.sawHosts, hosts)
	e.callCount++
	e.mu.Unlock()
	for _, f := range e.findings {
		emit(f)
	}
	if e.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return e.err
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func startService(t *testing.T, stages []scan.Stage, opts scan.Options) *scan.Service {
	t.Helper()
	repo := store.NewMemory(100)
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.QueueSize == 0 {
		opts.QueueSize = 10
	}
	opts.ProgressInterval = 10 * time.Millisecond
	svc := scan.NewService(repo, stages, opts, quiet)
	ctx, cancel := context.WithCancel(context.Background())
	svc.Start(ctx)
	t.Cleanup(func() { cancel(); svc.Wait() })
	return svc
}

func waitFinished(t *testing.T, svc *scan.Service, id string) scan.Scan {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sc, err := svc.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sc.Status == scan.StatusCompleted || sc.Status == scan.StatusFailed {
			return sc
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scan %s did not finish", id)
	return scan.Scan{}
}

func stage(id string, engines ...discovery.Engine) scan.Stage {
	return scan.Stage{ID: id, Label: "Running " + id, Engines: engines}
}

func TestScanPipeline(t *testing.T) {
	ct := &fakeEngine{name: "subdomains", findings: []discovery.Finding{
		{Host: "api.example.com", Source: discovery.SourceCT},
		{Host: "old.example.com", Source: discovery.SourceCT},
	}}
	broken := &fakeEngine{name: "other-source", err: errors.New("provider unavailable")}
	dns := &fakeEngine{name: "dns", findings: []discovery.Finding{
		{Host: "example.com", DNS: &discovery.DNSInfo{Resolved: true}},
		{Host: "api.example.com", DNS: &discovery.DNSInfo{Resolved: true}},
		{Host: "old.example.com", DNS: &discovery.DNSInfo{Resolved: false, Error: "no such host"}},
	}}
	probe := &fakeEngine{name: "http", findings: []discovery.Finding{
		{Host: "example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200}},
		{Host: "api.example.com", HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200}},
	}}
	crawl := &fakeEngine{name: "html", findings: []discovery.Finding{
		{URL: "https://example.com/", Source: discovery.SourceTarget, Hint: discovery.HintEntry,
			Response: &discovery.Response{Status: 200, ContentType: "text/html", Title: "Home"}},
		{URL: "https://api.example.com/v1/items", Source: discovery.SourceHTML},
		{URL: "https://docs.example.com/guide", Source: discovery.SourceHTML, Hint: discovery.HintLink},
	}}
	svc := startService(t, []scan.Stage{
		stage("subdomains", ct, broken),
		stage("resolve", dns),
		stage("probe", probe),
		stage("crawl", crawl),
	}, scan.Options{})

	sc, err := svc.Create(context.Background(), confirmed("https://Example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.Status != scan.StatusQueued || sc.ID == "" || sc.Domain != "example.com" {
		t.Fatalf("created scan = %+v", sc)
	}
	var ids []string
	for _, s := range sc.Steps {
		ids = append(ids, s.ID)
	}
	if !reflect.DeepEqual(ids, []string{"validate", "subdomains", "resolve", "probe", "crawl", "finalize"}) {
		t.Fatalf("steps = %v", ids)
	}
	if sc.Steps[0].Status != scan.StepDone {
		t.Error("validation step should be done at creation")
	}

	sc = waitFinished(t, svc, sc.ID)
	if sc.Status != scan.StatusCompleted {
		t.Fatalf("status = %s (%s)", sc.Status, sc.Error)
	}
	for _, s := range sc.Steps {
		// One of two passive sources failed; the stage still counts as done.
		if s.Status != scan.StepDone {
			t.Errorf("step %s = %s", s.ID, s.Status)
		}
	}
	if len(sc.Errors) != 1 || sc.Errors[0].Engine != "other-source" || sc.Errors[0].Stage != "subdomains" {
		t.Errorf("errors = %+v", sc.Errors)
	}

	// The target host is seeded before any engine runs; later stages see
	// hosts found by earlier ones.
	if got := ct.sawHosts[0]; !reflect.DeepEqual(got, []string{"example.com"}) {
		t.Errorf("first stage saw %v", got)
	}
	if got := dns.sawHosts[0]; !reflect.DeepEqual(got, []string{"api.example.com", "example.com", "old.example.com"}) {
		t.Errorf("resolve stage saw %v", got)
	}

	res, err := svc.Result(context.Background(), sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	c := res.Counts
	if c.Hosts != 4 || c.HostsResolved != 2 || c.HostsReachable != 2 || c.URLs != 3 || c.APIs != 1 {
		t.Errorf("counts = %+v", c)
	}
	byName := map[string]bool{}
	for _, h := range res.Hosts {
		byName[h.Hostname] = true
		if h.Hostname == "old.example.com" && (h.DNS == nil || h.DNS.Resolved || h.State != "discovered") {
			t.Errorf("old host = %+v", h)
		}
		if h.Hostname == "example.com" && !reflect.DeepEqual(h.Sources, []discovery.Source{discovery.SourceTarget}) {
			t.Errorf("apex sources = %v", h.Sources)
		}
	}
	if !byName["docs.example.com"] {
		t.Error("host discovered during crawl was not registered")
	}
	if res.Domain.Canonical != "example.com" || len(res.Errors) != 1 {
		t.Errorf("result domain/errors = %+v / %+v", res.Domain, res.Errors)
	}
}

func TestScanSeedsEnteredHostAndApex(t *testing.T) {
	e := &fakeEngine{name: "noop"}
	svc := startService(t, []scan.Stage{stage("s", e)}, scan.Options{})
	sc, _ := svc.Create(context.Background(), confirmed("https://www.example.com/docs"))
	waitFinished(t, svc, sc.ID)
	if got := e.sawHosts[0]; !reflect.DeepEqual(got, []string{"example.com", "www.example.com"}) {
		t.Errorf("seeded hosts = %v", got)
	}
}

func TestScanFailsWhenEveryEngineFails(t *testing.T) {
	svc := startService(t, []scan.Stage{
		stage("a", &fakeEngine{name: "a", err: errors.New("down")}),
		stage("b", &fakeEngine{name: "b", err: errors.New("down")}),
	}, scan.Options{})
	sc, err := svc.Create(context.Background(), confirmed("example.com"))
	if err != nil {
		t.Fatal(err)
	}
	sc = waitFinished(t, svc, sc.ID)
	if sc.Status != scan.StatusFailed || sc.Error == "" || sc.Steps[1].Status != scan.StepFailed {
		t.Errorf("scan = %+v", sc)
	}
	// Partial results are still stored.
	if _, err := svc.Result(context.Background(), sc.ID); err != nil {
		t.Errorf("result: %v", err)
	}
}

func TestScanTimeoutKeepsPartialResults(t *testing.T) {
	slow := &fakeEngine{name: "html", block: true, findings: []discovery.Finding{
		{URL: "https://example.com/", Source: discovery.SourceTarget, Hint: discovery.HintEntry},
	}}
	next := &fakeEngine{name: "js"}
	svc := startService(t, []scan.Stage{stage("crawl", slow), stage("js", next)}, scan.Options{ScanTimeout: 50 * time.Millisecond})
	sc, _ := svc.Create(context.Background(), confirmed("example.com"))
	sc = waitFinished(t, svc, sc.ID)
	if sc.Status != scan.StatusCompleted {
		t.Errorf("status = %s (%s), want completed with partial results", sc.Status, sc.Error)
	}
	if last := sc.Errors[len(sc.Errors)-1]; last.Engine != "scan" {
		t.Errorf("errors = %+v, want time limit notice", sc.Errors)
	}
	if sc.Steps[2].Status != scan.StepSkipped {
		t.Errorf("js step = %s, want skipped", sc.Steps[2].Status)
	}
	res, err := svc.Result(context.Background(), sc.ID)
	if err != nil || res.Counts.URLs != 1 {
		t.Errorf("partial result = %+v, err %v", res.Counts, err)
	}
}

func TestCreateRejectsInvalidTarget(t *testing.T) {
	svc := startService(t, nil, scan.Options{})
	if _, err := svc.Create(context.Background(), confirmed("http://127.0.0.1")); !errors.Is(err, discovery.ErrInvalidTarget) {
		t.Errorf("err = %v", err)
	}
}

func TestCreateQueueFull(t *testing.T) {
	// No workers started, queue of 1.
	svc := scan.NewService(store.NewMemory(10), nil, scan.Options{QueueSize: 1}, quiet)
	if _, err := svc.Create(context.Background(), confirmed("example.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), confirmed("example.org")); !errors.Is(err, scan.ErrQueueFull) {
		t.Errorf("err = %v, want ErrQueueFull", err)
	}
}

func confirmed(target string) scan.CreateRequest {
	return scan.CreateRequest{Target: target, AuthorizationConfirmed: true}
}

func TestCreateRequiresAuthorizationConfirmation(t *testing.T) {
	svc := startService(t, nil, scan.Options{})
	_, err := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com"})
	if !errors.Is(err, scan.ErrAuthorizationRequired) {
		t.Fatalf("err = %v, want ErrAuthorizationRequired", err)
	}
	sc, err := svc.Create(context.Background(), confirmed("example.com"))
	if err != nil || !sc.AuthorizationConfirmed {
		t.Fatalf("confirmed scan = %+v, %v", sc, err)
	}
}

// fetchingEngine makes n requests through the shared client using the
// scan's context, so the scan's request budget applies.
type fetchingEngine struct {
	client *fetch.Client
	url    string
	n      int
	errs   []error
}

func (e *fetchingEngine) Name() string { return "fetcher" }
func (e *fetchingEngine) Discover(ctx context.Context, _ discovery.Input, _ discovery.Emit) error {
	for i := 0; i < e.n; i++ {
		_, err := e.client.Get(ctx, e.url)
		e.errs = append(e.errs, err)
	}
	return nil
}

func TestScanRequestLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	e := &fetchingEngine{client: fetch.New(fetch.Options{AllowPrivate: true}), url: srv.URL, n: 5}
	svc := startService(t, []scan.Stage{stage("fetch", e)}, scan.Options{MaxRequests: 3})

	sc, _ := svc.Create(context.Background(), confirmed("example.com"))
	sc = waitFinished(t, svc, sc.ID)
	if sc.Requests != 3 {
		t.Errorf("requests = %d, want 3", sc.Requests)
	}
	blocked := 0
	for _, err := range e.errs {
		if errors.Is(err, fetch.ErrRequestLimit) {
			blocked++
		}
	}
	if blocked != 2 {
		t.Errorf("blocked = %d, want 2 (errs %v)", blocked, e.errs)
	}
	if last := sc.Errors[len(sc.Errors)-1]; last.Engine != "scan" || !strings.Contains(last.Message, "request limit") {
		t.Errorf("errors = %+v", sc.Errors)
	}
}
