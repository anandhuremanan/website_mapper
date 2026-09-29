package htmlcrawl

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"

	"websitemapper/internal/discovery"
	"websitemapper/internal/normalize"
)

// reference is a URL found in an HTML document.
type reference struct {
	URL    *url.URL
	Hint   discovery.Hint
	Method string
}

// page is what we extract from an HTML document.
type page struct {
	Title     string
	Generator string
	Refs      []reference
}

// extract parses an HTML document and returns its title, generator and all
// http(s) references resolved against pageURL (or <base href>, if present).
func extract(pageURL *url.URL, body []byte) page {
	var p page
	base := pageURL
	baseSet := false
	inTitle := false

	add := func(ref string, hint discovery.Hint, method string) {
		u, err := normalize.Resolve(base, ref)
		if err != nil {
			return
		}
		p.Refs = append(p.Refs, reference{URL: u, Hint: hint, Method: method})
	}

	z := html.NewTokenizer(bytes.NewReader(body))
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return p
		case html.TextToken:
			if inTitle && p.Title == "" {
				p.Title = strings.Join(strings.Fields(string(z.Text())), " ")
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = false
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			tok := z.Token()
			attr := attrs(tok)
			switch tok.Data {
			case "title":
				inTitle = tt == html.StartTagToken
			case "base":
				if href := attr["href"]; href != "" && !baseSet {
					if u, err := normalize.Resolve(pageURL, href); err == nil {
						base, baseSet = u, true
					}
				}
			case "meta":
				if strings.EqualFold(attr["name"], "generator") && p.Generator == "" {
					p.Generator = strings.TrimSpace(attr["content"])
				}
			case "a", "area":
				add(attr["href"], discovery.HintLink, "")
			case "link":
				if hint, ok := linkHint(attr); ok {
					add(attr["href"], hint, "")
				}
			case "script":
				add(attr["src"], discovery.HintScript, "")
			case "img":
				add(attr["src"], discovery.HintImage, "")
				for _, c := range srcset(attr["srcset"]) {
					add(c, discovery.HintImage, "")
				}
			case "source":
				add(attr["src"], discovery.HintMedia, "")
				for _, c := range srcset(attr["srcset"]) {
					add(c, discovery.HintImage, "")
				}
			case "video", "audio":
				add(attr["src"], discovery.HintMedia, "")
				add(attr["poster"], discovery.HintImage, "")
			case "iframe":
				add(attr["src"], discovery.HintFrame, "")
			case "form":
				// Forms are recorded, never submitted.
				if action := attr["action"]; action != "" {
					method := strings.ToUpper(strings.TrimSpace(attr["method"]))
					if method != "POST" {
						method = "GET"
					}
					add(action, discovery.HintForm, method)
				}
			}
		}
	}
}

func attrs(t html.Token) map[string]string {
	m := make(map[string]string, len(t.Attr))
	for _, a := range t.Attr {
		if _, dup := m[a.Key]; !dup {
			m[a.Key] = a.Val
		}
	}
	return m
}

func linkHint(attr map[string]string) (discovery.Hint, bool) {
	rels := strings.Fields(strings.ToLower(attr["rel"]))
	has := func(r string) bool {
		for _, x := range rels {
			if x == r {
				return true
			}
		}
		return false
	}
	switch {
	case has("stylesheet"):
		return discovery.HintStylesheet, true
	case has("icon"), has("apple-touch-icon"), has("mask-icon"):
		return discovery.HintImage, true
	case has("modulepreload"):
		return discovery.HintScript, true
	case has("manifest"):
		return discovery.HintManifest, true
	case has("preload"), has("prefetch"):
		switch strings.ToLower(attr["as"]) {
		case "script":
			return discovery.HintScript, true
		case "style":
			return discovery.HintStylesheet, true
		case "font":
			return discovery.HintFont, true
		case "image":
			return discovery.HintImage, true
		case "document":
			return discovery.HintLink, true
		}
	case has("canonical"), has("alternate"), has("next"), has("prev"):
		return discovery.HintLink, true
	}
	return "", false
}

// srcset returns the URLs of a srcset attribute ("a.png 1x, b.png 2x").
func srcset(v string) []string {
	if v == "" {
		return nil
	}
	var out []string
	for _, candidate := range strings.Split(v, ",") {
		if f := strings.Fields(candidate); len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}
