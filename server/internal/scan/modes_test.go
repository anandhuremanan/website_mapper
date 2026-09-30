package scan_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"websitemapper/internal/scan"
)

// modePipeline mirrors the production pipeline's shape: shared stages,
// light/full-only stages, and per-mode follow-up stages with the same ID.
func modePipeline(g *gateEngine) ([]scan.Stage, map[string]*fakeEngine) {
	e := map[string]*fakeEngine{}
	for _, n := range []string{"subdomains", "archive", "dns", "http", "sitemap", "html"} {
		e[n] = &fakeEngine{name: n}
	}
	light, full := []scan.Mode{scan.ModeLight}, []scan.Mode{scan.ModeFull}
	both := []scan.Mode{scan.ModeLight, scan.ModeFull}
	stages := []scan.Stage{
		{ID: "subdomains", Label: "Subdomains", Engines: nil},
		{ID: "archive", Label: "Archive", Engines: nil},
		{ID: "resolve", Label: "Resolve", Engines: nil},
		{ID: "probe", Label: "Probe", Engines: nil, Modes: both},
		{ID: "sitemaps", Label: "Sitemaps", Engines: nil, Modes: both},
		{ID: "crawl", Label: "Crawl", Engines: nil, Modes: full},
		{ID: "follow-up", Label: "Follow-up", Engines: nil, Modes: light},
		{ID: "follow-up", Label: "Follow-up", Engines: nil, Modes: full},
	}
	stages[0].Engines = append(stages[0].Engines, e["subdomains"])
	stages[1].Engines = append(stages[1].Engines, e["archive"])
	stages[2].Engines = append(stages[2].Engines, e["dns"])
	stages[3].Engines = append(stages[3].Engines, e["http"])
	stages[4].Engines = append(stages[4].Engines, e["sitemap"])
	stages[5].Engines = append(stages[5].Engines, e["html"], g)
	stages[6].Engines = append(stages[6].Engines, e["dns"], e["http"], e["sitemap"])
	stages[7].Engines = append(stages[7].Engines, e["dns"], e["http"], e["sitemap"], e["html"])
	return stages, e
}

func stepIDs(sc scan.Scan) []string {
	var ids []string
	for _, s := range sc.Steps {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestModesRunTheirStages(t *testing.T) {
	g := newGate()
	close(g.release)
	stages, engines := modePipeline(g)
	svc := startService(t, stages, scan.Options{MaxRunning: 3})

	want := map[scan.Mode][]string{
		scan.ModePassive: {"validate", "subdomains", "archive", "resolve", "finalize"},
		scan.ModeLight:   {"validate", "subdomains", "archive", "resolve", "probe", "sitemaps", "follow-up", "finalize"},
		scan.ModeFull:    {"validate", "subdomains", "archive", "resolve", "probe", "sitemaps", "crawl", "follow-up", "finalize"},
	}
	for _, m := range []scan.Mode{scan.ModePassive, scan.ModeLight, scan.ModeFull} {
		before := map[string]int{}
		for n, e := range engines {
			before[n] = e.callCount
		}
		sc, err := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com", Mode: m})
		if err != nil {
			t.Fatal(err)
		}
		if sc.Mode != m || !reflect.DeepEqual(stepIDs(sc), want[m]) {
			t.Errorf("%s: mode %s steps %v", m, sc.Mode, stepIDs(sc))
		}
		done := waitStatus(t, svc, sc.ID, scan.StatusCompleted)
		res, _ := svc.Result(context.Background(), done.ID)
		if res.Domain.Mode != m {
			t.Errorf("%s: result mode %s", m, res.Domain.Mode)
		}
		ran := func(n string) int { return engines[n].callCount - before[n] }
		switch m {
		case scan.ModePassive:
			if ran("http")+ran("sitemap")+ran("html") != 0 {
				t.Errorf("passive contacted the site: http %d sitemap %d html %d", ran("http"), ran("sitemap"), ran("html"))
			}
		case scan.ModeLight:
			if ran("html") != 0 || ran("sitemap") != 2 || ran("http") != 2 {
				t.Errorf("light: html %d sitemap %d http %d", ran("html"), ran("sitemap"), ran("http"))
			}
		case scan.ModeFull:
			if ran("html") != 2 {
				t.Errorf("full: html %d", ran("html"))
			}
		}
	}
}

func TestDefaultAndInvalidMode(t *testing.T) {
	g := newGate()
	defer close(g.release)
	stages, _ := modePipeline(g)
	svc := startService(t, stages, scan.Options{MaxRunning: 3, DefaultMode: scan.ModeLight})
	if sc := mustCreate(t, svc, "example.com"); sc.Mode != scan.ModeLight {
		t.Errorf("default mode = %s", sc.Mode)
	}
	if _, err := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com", Mode: "deep"}); !errors.Is(err, scan.ErrInvalidMode) {
		t.Errorf("err = %v", err)
	}
}

func TestDifferentModesDoNotCoalesce(t *testing.T) {
	g := newGate()
	defer close(g.release)
	stages, _ := modePipeline(g)
	svc := startService(t, stages, scan.Options{MaxRunning: 3})
	light, _ := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com", Mode: scan.ModeFull})
	full, _ := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com", Mode: scan.ModePassive})
	again, _ := svc.Create(context.Background(), scan.CreateRequest{Target: "example.com", Mode: scan.ModeFull})
	if light.ID == full.ID || full.Coalesced {
		t.Errorf("different modes coalesced: %s %s", light.ID, full.ID)
	}
	if again.ID != light.ID || !again.Coalesced {
		t.Errorf("same mode did not coalesce: %+v", again)
	}
}
