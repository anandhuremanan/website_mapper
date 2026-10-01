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

// techSet collects technology evidence while a scan runs, so detecting
// technologies never needs a pass over every URL.
type techSet map[string][]string

// add records evidence for a technology. Each keeps its maxEvidence
// alphabetically first evidence strings, so the outcome does not depend on
// the order findings arrive in.
func (t techSet) add(name, ev string) {
	evs := t[name]
	i := sort.SearchStrings(evs, ev)
	if i < len(evs) && evs[i] == ev {
		return
	}
	if i >= maxEvidence {
		return
	}
	evs = append(evs, "")
	copy(evs[i+1:], evs[i:])
	evs[i] = ev
	if len(evs) > maxEvidence {
		evs = evs[:maxEvidence]
	}
	t[name] = evs
}

// addPath applies the path rules to a newly recorded URL's path.
func (t techSet) addPath(path string) {
	for _, r := range pathRules {
		if strings.Contains(path, r.fragment) {
			t.add(r.name, "URL path contains "+r.fragment)
		}
	}
}

// addResponse applies the header and generator rules to a response.
func (t techSet) addResponse(server, poweredBy, generator string) {
	if server != "" {
		t.add(headerProduct(server), "Server: "+server)
	}
	if poweredBy != "" {
		t.add(headerProduct(poweredBy), "X-Powered-By: "+poweredBy)
	}
	if generator != "" {
		t.add(headerProduct(generator), `<meta name="generator" content="`+generator+`">`)
	}
}

// list returns the technologies detected so far, sorted by name.
func (t techSet) list() []Technology {
	out := make([]Technology, 0, len(t))
	for name, ev := range t {
		out = append(out, Technology{Name: name, Evidence: append([]string(nil), ev...)})
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
