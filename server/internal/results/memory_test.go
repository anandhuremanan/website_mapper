package results

import (
	"fmt"
	"testing"

	"websitemapper/internal/discovery"
)

// buildLargeScan fills an aggregator the way a large scan does: hosts from
// certificate logs with DNS/probe/crawl observations, and URLs referenced
// from HTML (a quarter of them fetched, with metadata).
func buildLargeScan(hosts, urlsPerHost int) *Aggregator {
	tgt, _ := discovery.ParseTarget("example.com")
	a := NewAggregator(tgt, Limits{}, newMemURLs())
	for h := 0; h < hosts; h++ {
		name := fmt.Sprintf("host-%05d.example.com", h)
		a.Add(discovery.Finding{Host: name, Source: discovery.SourceCT})
		a.Add(discovery.Finding{Host: name, DNS: &discovery.DNSInfo{Resolved: true, Addresses: []string{"93.184.216.34"}}})
		a.Add(discovery.Finding{Host: name, HTTP: &discovery.HTTPInfo{Reachable: true, Status: 200, URL: "https://" + name + "/", Title: "Example host title"}})
		a.Add(discovery.Finding{Host: name, Crawl: &discovery.CrawlInfo{Requests: urlsPerHost / 4}})
		for u := 0; u < urlsPerHost; u++ {
			f := discovery.Finding{
				URL:    fmt.Sprintf("https://%s/section-%d/some-article-slug-%d", name, u%20, u),
				Source: discovery.SourceHTML, Hint: discovery.HintLink,
				From: fmt.Sprintf("https://%s/section-%d/", name, u%20),
			}
			if u%4 == 0 {
				f.Response = &discovery.Response{Status: 200, ContentType: "text/html", Title: "An article title of typical length", Server: "nginx"}
			}
			a.Add(f)
		}
	}
	return a
}

// BenchmarkCounts measures one progress tick on a large scan.
func BenchmarkCounts(b *testing.B) {
	for _, hosts := range []int{1000, 10000} {
		a := buildLargeScan(hosts, 5)
		b.Run(fmt.Sprintf("hosts=%d", hosts), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = a.Counts()
			}
		})
	}
}
