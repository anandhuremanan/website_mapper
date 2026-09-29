package discovery

import (
	"sort"
	"sync"
)

// PrioritizeHosts orders hosts for bounded processing: the host the user
// entered, then the apex and www, then hosts seen through several sources or
// linked from content, then alphabetically. The order is deterministic so
// the same inputs select the same hosts when a limit applies.
func PrioritizeHosts(hosts []HostView, t Target) {
	start := HostOf(t.StartURL)
	rank := func(h HostView) int {
		switch h.Name {
		case start:
			return 0
		case t.Domain:
			return 1
		case "www." + t.Domain:
			return 2
		}
		for _, s := range h.Sources {
			if s != SourceCT {
				return 3 // linked from content or a redirect
			}
		}
		return 4
	}
	sort.SliceStable(hosts, func(i, j int) bool {
		ri, rj := rank(hosts[i]), rank(hosts[j])
		if ri != rj {
			return ri < rj
		}
		return hosts[i].Name < hosts[j].Name
	})
}

// ForEachLimited runs fn for every item with at most n running at once.
func ForEachLimited[T any](items []T, n int, fn func(T)) {
	if n < 1 {
		n = 1
	}
	sem := make(chan struct{}, n)
	var wg sync.WaitGroup
	for _, it := range items {
		sem <- struct{}{}
		wg.Add(1)
		go func(it T) {
			defer func() { <-sem; wg.Done() }()
			fn(it)
		}(it)
	}
	wg.Wait()
}
