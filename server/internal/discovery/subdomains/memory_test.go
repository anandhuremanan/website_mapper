package subdomains

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"websitemapper/internal/discovery"
)

// fakeMemory is a Memory in a map.
type fakeMemory struct {
	mu    sync.Mutex
	hosts map[string][]string
	at    map[string]time.Time
}

func newFakeMemory() *fakeMemory {
	return &fakeMemory{hosts: map[string][]string{}, at: map[string]time.Time{}}
}

func (m *fakeMemory) Load(source, domain string) ([]string, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hosts[source+"|"+domain]
	return h, m.at[source+"|"+domain], ok
}

func (m *fakeMemory) Save(source, domain string, hosts []string) {
	m.put(source, domain, hosts, time.Now())
}

func (m *fakeMemory) put(source, domain string, hosts []string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hosts[source+"|"+domain], m.at[source+"|"+domain] = hosts, at
}

// scripted is a provider whose answer the test sets.
type scripted struct {
	name  string
	names []string
	err   error
	calls int
	block chan struct{} // if set, Discover waits for it
}

func (s *scripted) Name() string                 { return s.name }
func (s *scripted) Provenance() discovery.Source { return discovery.SourceCT }
func (s *scripted) Discover(ctx context.Context, _ string) ([]string, error) {
	s.calls++
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.names, s.err
}

func collect(t *testing.T, e *Engine, ctx context.Context) ([]discovery.Finding, error) {
	t.Helper()
	var mu sync.Mutex
	var got []discovery.Finding
	err := e.Discover(ctx, discovery.Input{Target: target(t, "example.com")}, func(f discovery.Finding) {
		mu.Lock()
		got = append(got, f)
		mu.Unlock()
	})
	sort.Slice(got, func(i, j int) bool { return got[i].Host < got[j].Host })
	return got, err
}

func hostsOf(fs []discovery.Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Host)
	}
	return out
}

func withMemory(src Source, m Memory) *Engine {
	return New([]Source{src}, time.Minute, quiet).WithCache(CacheOptions{
		Cache: NewCache(1 << 20), TTL: 6 * time.Hour, RateLimitTTL: time.Minute, FailureTTL: time.Minute, Memory: m,
	})
}

func TestAnswersAreRemembered(t *testing.T) {
	m := newFakeMemory()
	src := &scripted{name: "crt.sh", names: []string{"api.example.com", "*.example.com", "other.org"}}
	if got, err := collect(t, withMemory(src, m), context.Background()); err != nil || len(got) != 2 {
		t.Fatalf("first scan: %v, %v", hostsOf(got), err)
	}
	// What was saved is the filtered, in-scope list.
	if hosts, _, ok := m.Load("crt.sh", "example.com"); !ok || !reflect.DeepEqual(hosts, []string{"api.example.com", "example.com"}) {
		t.Errorf("remembered = %v, %v", hosts, ok)
	}

	// After a restart (a new engine with an empty cache), a fresh answer in
	// the memory means the provider is not asked at all.
	src2 := &scripted{name: "crt.sh", names: []string{"changed.example.com"}}
	got, err := collect(t, withMemory(src2, m), context.Background())
	if err != nil || src2.calls != 0 || !reflect.DeepEqual(hostsOf(got), []string{"api.example.com", "example.com"}) {
		t.Errorf("after restart: %v, err %v, provider asked %d times", hostsOf(got), err, src2.calls)
	}
	if got[0].CachedAt.IsZero() {
		t.Error("a remembered host must say when it was really found")
	}
}

func TestRememberedAnswerCoversAnOutage(t *testing.T) {
	m := newFakeMemory()
	old := time.Now().Add(-9 * 24 * time.Hour) // older than the cache TTL: the provider is asked
	m.put("crt.sh", "example.com", []string{"api.example.com", "old.example.com"}, old)
	src := &scripted{name: "crt.sh", err: errors.New("crt.sh returned HTTP 502")}

	got, err := collect(t, withMemory(src, m), context.Background())
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "HTTP 502; used its answer from "+old.UTC().Format("2 Jan 2006")) {
		t.Errorf("err = %v", err)
	}
	if src.calls != 1 || !reflect.DeepEqual(hostsOf(got), []string{"api.example.com", "old.example.com"}) {
		t.Errorf("hosts = %v, provider asked %d times", hostsOf(got), src.calls)
	}
	if !got[0].CachedAt.Equal(old) {
		t.Errorf("CachedAt = %v, want %v", got[0].CachedAt, old)
	}

	// Without anything remembered, the failure is a failure.
	got, err = collect(t, withMemory(&scripted{name: "crt.sh", err: errors.New("down")}, newFakeMemory()), context.Background())
	if err == nil || errors.As(err, &partial) || len(got) != 0 {
		t.Errorf("nothing remembered: %v, %v", hostsOf(got), err)
	}
}

func TestRememberedAnswerIsUsedWhenTheScanMovesOn(t *testing.T) {
	m := newFakeMemory()
	old := time.Now().Add(-3 * 24 * time.Hour)
	m.put("crt.sh", "example.com", []string{"api.example.com"}, old)
	src := &scripted{name: "crt.sh", names: []string{"api.example.com", "new.example.com"}, block: make(chan struct{})}
	e := withMemory(src, m)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	got, err := collect(t, e, ctx)
	var partial *discovery.PartialError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "used its answer from") {
		t.Errorf("err = %v", err)
	}
	if !reflect.DeepEqual(hostsOf(got), []string{"api.example.com"}) {
		t.Errorf("hosts = %v", hostsOf(got))
	}

	// The provider answers later; that answer replaces the remembered one.
	close(src.block)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if hosts, _, _ := m.Load("crt.sh", "example.com"); len(hosts) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late answer was not remembered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// fakeDB stands in for crt.sh's database.
type fakeDB struct {
	names []string
	err   error
	calls int
}

func (d *fakeDB) Names(context.Context, string) ([]string, error) {
	d.calls++
	return d.names, d.err
}

func TestCRTShAsksTheDatabaseFirst(t *testing.T) {
	site, req := provider(t, 200, `[{"name_value":"web.example.com"}]`)
	c := NewCRTSh(client())
	c.BaseURL, c.RetryDelay = site.URL, time.Millisecond

	// The database answers: the website is not asked.
	db := &fakeDB{names: []string{"db.example.com"}}
	c.DB = db
	if names, err := c.Discover(context.Background(), "example.com"); err != nil || !reflect.DeepEqual(names, []string{"db.example.com"}) || *req != "" {
		t.Errorf("database answer: %v, %v, website asked: %q", names, err, *req)
	}

	// The database is busy: the website answers instead.
	db.err = errors.New("crt.sh database is busy (no free connections)")
	if names, err := c.Discover(context.Background(), "example.com"); err != nil || !reflect.DeepEqual(names, []string{"web.example.com"}) {
		t.Errorf("website fallback: %v, %v", names, err)
	}

	// Both fail: the message names both.
	down, _ := provider(t, 502, "")
	c.BaseURL = down.URL
	_, err := c.Discover(context.Background(), "example.com")
	if err == nil || !strings.Contains(err.Error(), "database is busy") || !strings.Contains(err.Error(), "502") {
		t.Errorf("both down: %v", err)
	}
}

func TestCRTShDatabaseRefusesOddDomains(t *testing.T) {
	db := &CRTShDB{DSN: "host=127.0.0.1 port=1 connect_timeout=1 sslmode=disable"}
	for _, d := range []string{"exa'mple.com", "example.com; DROP TABLE x", "a b.com", "", "-x.com", `a\.com`} {
		if _, err := db.Names(context.Background(), d); err == nil || !strings.Contains(err.Error(), "not a plain hostname") {
			t.Errorf("Names(%q) = %v, want it refused before any query", d, err)
		}
	}
}
