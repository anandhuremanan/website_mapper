package results

import (
	"sort"

	"websitemapper/internal/classify"
)

// memURLs is a URLStore kept in a map, for testing the Aggregator without a
// database.
type memURLs struct {
	recs map[memKey]*URLRecord
}

type memKey struct {
	host         int64
	path, origin string
}

func newMemURLs() *memURLs { return &memURLs{recs: map[memKey]*URLRecord{}} }

func (m *memURLs) Insert(r *URLRecord) (bool, error) {
	k := memKey{r.HostID, r.Path, r.Origin}
	if _, ok := m.recs[k]; ok {
		return false, nil
	}
	m.recs[k] = r
	return true, nil
}

func (m *memURLs) Get(hostID int64, path, origin string) (*URLRecord, bool, error) {
	r, ok := m.recs[memKey{hostID, path, origin}]
	return r, ok, nil
}

func (m *memURLs) Update(*URLRecord) error { return nil }

func (m *memURLs) PendingPages(fn func(hostID int64, path, origin string)) error {
	for k, r := range m.recs {
		if r.Pending() {
			fn(k.host, k.path, k.origin)
		}
	}
	return nil
}

func (m *memURLs) Flush() error { return nil }

// fullResult is a.Result() with every host's URLs filled in from the
// aggregator's memURLs, in stored order (path, then origin).
func fullResult(a *Aggregator) Result {
	res := a.Result()
	m := a.store.(*memURLs)
	for i := range res.Hosts {
		h := &res.Hosts[i]
		var recs []*URLRecord
		for k, r := range m.recs {
			if k.host == h.ID {
				recs = append(recs, r)
			}
		}
		sort.Slice(recs, func(i, j int) bool {
			if recs[i].Path != recs[j].Path {
				return recs[i].Path < recs[j].Path
			}
			return recs[i].Origin < recs[j].Origin
		})
		for _, r := range recs {
			h.URLs = append(h.URLs, r.View(h.Hostname))
		}
	}
	return res
}

// recount computes a result's counts from scratch, to check the counters
// the Aggregator maintains incrementally.
func recount(r Result) Counts {
	c := Counts{}
	c.Limits.HostsOmitted = r.HostsOmitted
	for _, h := range r.Hosts {
		countHost(&c, h.State, h.DNS, h.HTTP, h.Crawl, h.Omitted)
		if h.FromCache {
			c.Cache.Hosts++
		}
		hc := HostCounts{}
		for _, u := range h.URLs {
			c.URLs++
			hc.URLs++
			switch u.Type {
			case classify.TypePage:
				c.Pages++
				hc.Pages++
			case classify.TypeAPI:
				c.APIs++
				hc.APIs++
			case classify.TypeAsset:
				c.Assets++
				hc.Assets++
				if u.AssetKind == classify.AssetJavaScript {
					c.JavaScript++
				}
			}
			switch {
			case u.Error != "":
				c.URLsFailed++
			case u.Fetched:
				c.URLsFetched++
				if u.CachedAt != nil {
					c.Cache.Pages++
				}
			}
		}
		if hc != h.Counts {
			// Make a mismatch in a host's own counters visible to callers
			// comparing the totals.
			c.URLs = -1
		}
	}
	return c
}
