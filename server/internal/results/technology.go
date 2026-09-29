package results

import (
	"regexp"
	"sort"
	"strings"
)

// versionPattern matches version tokens such as "6.4.2" or "v0.120.0".
var versionPattern = regexp.MustCompile(`^[vV]?[0-9][0-9A-Za-z.\-]*$`)

// maxEvidence bounds how many evidence strings are kept per technology.
const maxEvidence = 3

// pathRule detects a technology from a path fragment seen in any URL.
type pathRule struct {
	name     string
	fragment string
}

// pathRules are deliberately simple and evidence-based. Add rules here;
// anything more sophisticated belongs in a dedicated engine.
var pathRules = []pathRule{
	{"Next.js", "/_next/"},
	{"SvelteKit", "/_app/immutable/"},
	{"Nuxt", "/_nuxt/"},
	{"Astro", "/_astro/"},
	{"Gatsby", "/page-data/"},
	{"WordPress", "/wp-content/"},
	{"WordPress", "/wp-includes/"},
	{"Drupal", "/sites/default/files/"},
	{"Shopify", "/cdn/shop/"},
}

// detectTechnologies runs path, header and generator rules over all URLs
// and host probes. Caller holds the aggregator lock.
func detectTechnologies(urls map[string]*urlEntry, hosts map[string]*hostEntry) []Technology {
	found := map[string][]string{}
	addEvidence := func(name, ev string) {
		evs := found[name]
		if len(evs) < maxEvidence && !contains(evs, ev) {
			found[name] = append(evs, ev)
		}
	}

	keys := make([]string, 0, len(urls))
	for k := range urls {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic evidence order

	for _, k := range keys {
		e := urls[k]
		for _, r := range pathRules {
			if strings.Contains(e.u.Path, r.fragment) {
				addEvidence(r.name, "URL path contains "+r.fragment)
			}
		}
		if e.resp == nil {
			continue
		}
		if s := e.resp.Server; s != "" {
			addEvidence(headerProduct(s), "Server: "+s)
		}
		if p := e.resp.PoweredBy; p != "" {
			addEvidence(headerProduct(p), "X-Powered-By: "+p)
		}
		if g := e.resp.Generator; g != "" {
			addEvidence(headerProduct(g), `<meta name="generator" content="`+g+`">`)
		}
	}

	names := make([]string, 0, len(hosts))
	for n := range hosts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if h := hosts[n].http; h != nil && h.Server != "" {
			addEvidence(headerProduct(h.Server), "Server: "+h.Server)
		}
	}

	out := make([]Technology, 0, len(found))
	for name, ev := range found {
		out = append(out, Technology{Name: name, Evidence: ev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// headerProduct extracts a product name from values such as "nginx/1.25.3",
// "Apache/2.4 (Ubuntu)", "Google Frontend", "WordPress 6.4.2" or
// "Discourse 3.1.1 - https://github.com/discourse/discourse".
func headerProduct(v string) string {
	name := strings.TrimSpace(v)
	if i := strings.Index(name, " - "); i > 0 {
		name = name[:i]
	}
	if i := strings.IndexAny(name, "/("); i > 0 {
		name = name[:i]
	}
	fields := strings.Fields(name)
	for len(fields) > 1 && versionPattern.MatchString(fields[len(fields)-1]) {
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return strings.TrimSpace(v)
	}
	return strings.Join(fields, " ")
}
