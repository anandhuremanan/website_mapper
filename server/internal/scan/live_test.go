package scan_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/results"
	"websitemapper/internal/scan"
)

// readURLs lists every URL of a scan's current result.
func readURLs(t *testing.T, svc *scan.Service, id string) (scan.Result, []string) {
	t.Helper()
	r, err := svc.OpenResult(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var urls []string
	if _, err := r.URLs(context.Background(), scan.URLQuery{}, func(u results.URL) error {
		urls = append(urls, u.URL)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return r.Summary(), urls
}

// TestRunningScanCanBeRead: a scan's result is readable from the moment it
// runs, and shows what has been found so far.
func TestRunningScanCanBeRead(t *testing.T) {
	g := newGate()
	found := &fakeEngine{name: "sitemap", findings: []discovery.Finding{
		{URL: "https://example.com/a", Source: discovery.SourceSitemap, Hint: discovery.HintSitemap},
		{Host: "api.example.com", Source: discovery.SourceCT},
	}}
	svc := startService(t, []scan.Stage{stage("sitemaps", found), stage("crawl", g)},
		scan.Options{SnapshotInterval: time.Millisecond})
	sc := mustCreate(t, svc, "example.com")
	waitFor(t, func() bool { return g.didStart("example.com") })

	// The scan is blocked in its second stage. A snapshot follows shortly.
	waitFor(t, func() bool {
		_, urls := readURLs(t, svc, sc.ID)
		return len(urls) == 2
	})
	sum, urls := readURLs(t, svc, sc.ID)
	if !reflect.DeepEqual(urls, []string{"https://example.com/a", "https://example.com/partial"}) {
		t.Errorf("urls while running = %v", urls)
	}
	if sum.Status != scan.StatusRunning || sum.Counts.URLs != 2 || sum.Counts.Hosts != 2 {
		t.Errorf("summary while running = %+v", sum)
	}
	r, err := svc.OpenResult(context.Background(), sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := r.Hosts(context.Background(), scan.HostQuery{})
	r.Close()
	if err != nil || len(hosts.Hosts) != 2 || hosts.Hosts[0].Hostname != "example.com" || hosts.Hosts[0].Counts.URLs != 2 {
		t.Errorf("hosts while running = %+v, %v", hosts, err)
	}

	// A reader that is still open when the scan finishes does not stop the
	// result from being saved.
	open, err := svc.OpenResult(context.Background(), sc.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(g.release)
	waitStatus(t, svc, sc.ID, scan.StatusCompleted)
	open.Close()
	if sum, urls := readURLs(t, svc, sc.ID); sum.Status != scan.StatusCompleted || len(urls) != 2 {
		t.Errorf("final result = %+v, %v", sum, urls)
	}
}

// A queued scan has nothing to read yet.
func TestQueuedScanHasNoResult(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{MaxRunning: 1})
	mustCreate(t, svc, "a.com")
	queued := mustCreate(t, svc, "b.com")
	if _, err := svc.OpenResult(context.Background(), queued.ID); !errors.Is(err, scan.ErrNotFound) {
		t.Errorf("OpenResult of a queued scan: %v", err)
	}
	close(g.release)
}

// TestBackgroundStage: a background stage runs alongside the stages after
// it, and a joining stage sees what it found.
func TestBackgroundStage(t *testing.T) {
	archive := newGate() // blocks until released
	ct := &fakeEngine{name: "subdomains", findings: []discovery.Finding{{Host: "api.example.com", Source: discovery.SourceCT}}}
	dns := &fakeEngine{name: "dns"}
	late := &fakeEngine{name: "dns"}

	bg := stage("archive", archive)
	bg.Background = true
	follow := stage("follow-up", late)
	follow.Join = true
	svc := startService(t, []scan.Stage{bg, stage("subdomains", ct), stage("resolve", dns), follow}, scan.Options{})
	sc := mustCreate(t, svc, "example.com")

	// The foreground stages finish while the background stage is still
	// running; the joining stage waits for it.
	waitFor(t, func() bool { return dns.calls() == 1 && archive.didStart("example.com") })
	time.Sleep(20 * time.Millisecond)
	if late.calls() != 0 {
		t.Fatal("the joining stage ran before the background stage finished")
	}
	running, _ := svc.Get(context.Background(), sc.ID)
	steps := map[string]scan.StepStatus{}
	for _, st := range running.Steps {
		steps[st.ID] = st.Status
	}
	if steps["archive"] != scan.StepRunning || steps["subdomains"] != scan.StepDone || steps["resolve"] != scan.StepDone || steps["follow-up"] != scan.StepPending {
		t.Errorf("steps while the background stage runs = %v", steps)
	}

	close(archive.release)
	done := waitStatus(t, svc, sc.ID, scan.StatusCompleted)
	for _, st := range done.Steps {
		if st.Status != scan.StepDone {
			t.Errorf("step %s = %s", st.ID, st.Status)
		}
	}
	// The joining stage saw the URL's host found by the background stage
	// (already known here) and ran exactly once, after it.
	if late.calls() != 1 {
		t.Errorf("joining stage ran %d times", late.calls())
	}
	if _, urls := readURLs(t, svc, sc.ID); !reflect.DeepEqual(urls, []string{"https://example.com/partial"}) {
		t.Errorf("urls = %v", urls)
	}
}

// A scan whose only unfinished work is a background stage still waits for
// it, and cancelling stops it.
func TestBackgroundStageIsCancelled(t *testing.T) {
	archive := newGate()
	bg := stage("archive", archive)
	bg.Background = true
	svc := startService(t, []scan.Stage{bg, stage("resolve", &fakeEngine{name: "dns"})}, scan.Options{})
	sc := mustCreate(t, svc, "example.com")
	waitFor(t, func() bool { return archive.didStart("example.com") })
	if _, err := svc.Cancel(context.Background(), sc.ID, sc.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	done := waitStatus(t, svc, sc.ID, scan.StatusCancelled)
	if done.Steps[1].Status != scan.StepStopped {
		t.Errorf("background step after cancel = %s", done.Steps[1].Status)
	}
}

func TestRecentScanIsReused(t *testing.T) {
	eng := &fakeEngine{name: "sitemap", findings: []discovery.Finding{
		{URL: "https://example.com/a", Source: discovery.SourceSitemap, Hint: discovery.HintSitemap},
	}}
	svc := startService(t, []scan.Stage{stage("sitemaps", eng)}, scan.Options{ReuseTTL: time.Hour})
	ctx := context.Background()

	first := mustCreate(t, svc, "example.com")
	if first.Reused {
		t.Fatal("the first scan cannot be a reused one")
	}
	waitStatus(t, svc, first.ID, scan.StatusCompleted)

	// The same target and mode again: the finished scan, at once.
	again := mustCreate(t, svc, "https://example.com/")
	if again.ID != first.ID || !again.Reused || again.Status != scan.StatusCompleted || again.SubscriptionID != "" {
		t.Errorf("second request = %+v", again)
	}
	if eng.calls() != 1 {
		t.Errorf("engine ran %d times, want 1", eng.calls())
	}
	if st := svc.Stats(); st.ReusedRequests != 1 {
		t.Errorf("stats = %+v", st)
	}

	// Another mode, another target, or an explicit fresh scan are new scans.
	other, err := svc.Create(ctx, scan.CreateRequest{Target: "example.com", Mode: scan.ModeFull})
	if err != nil || other.ID == first.ID || other.Reused {
		t.Errorf("other mode = %+v, %v", other, err)
	}
	if www := mustCreate(t, svc, "www.example.com"); www.ID == first.ID || www.Reused {
		t.Errorf("other target = %+v", www)
	}
	fresh, err := svc.Create(ctx, scan.CreateRequest{Target: "example.com", Fresh: true})
	if err != nil || fresh.ID == first.ID || fresh.Reused {
		t.Errorf("fresh = %+v, %v", fresh, err)
	}
	waitStatus(t, svc, fresh.ID, scan.StatusCompleted)
	// The fresh scan is now the one that is reused.
	if latest := mustCreate(t, svc, "example.com"); latest.ID != fresh.ID || !latest.Reused {
		t.Errorf("after a fresh scan = %+v", latest)
	}
}

func TestOnlyCompleteScansAreReused(t *testing.T) {
	g := newGate()
	svc := startService(t, []scan.Stage{stage("crawl", g)}, scan.Options{ReuseTTL: time.Hour})
	sc := mustCreate(t, svc, "example.com")
	waitFor(t, func() bool { return g.didStart("example.com") })
	if _, err := svc.Cancel(context.Background(), sc.ID, sc.SubscriptionID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, sc.ID, scan.StatusCancelled)
	if next := mustCreate(t, svc, "example.com"); next.ID == sc.ID || next.Reused {
		t.Errorf("a cancelled scan was reused: %+v", next)
	}
}

func TestReuseExpires(t *testing.T) {
	eng := &fakeEngine{name: "sitemap"}
	svc := startService(t, []scan.Stage{stage("sitemaps", eng)}, scan.Options{ReuseTTL: 30 * time.Millisecond})
	first := mustCreate(t, svc, "example.com")
	waitStatus(t, svc, first.ID, scan.StatusCompleted)
	time.Sleep(60 * time.Millisecond)
	if next := mustCreate(t, svc, "example.com"); next.ID == first.ID || next.Reused {
		t.Errorf("an expired scan was reused: %+v", next)
	}
}
