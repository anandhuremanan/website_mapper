package classify

import (
	"strings"
	"testing"

	"websitemapper/internal/discovery"
)

func TestClassify(t *testing.T) {
	link := []discovery.Hint{discovery.HintLink}
	tests := []struct {
		name     string
		in       Input
		typ      Type
		kind     AssetKind
		evidence string
	}{
		{"html response", Input{Path: "/api/docs", Hints: link, Status: 200, ContentType: "text/html"}, TypePage, "", "text/html"},
		{"json response", Input{Path: "/data", Status: 200, ContentType: "application/json"}, TypeAPI, "", "application/json"},
		{"problem json", Input{Path: "/x", Status: 200, ContentType: "application/problem+json"}, TypeAPI, "", "problem+json"},
		{"404 ignores content type", Input{Path: "/api/users", Status: 404, ContentType: "text/html"}, TypeAPI, "", "not verified"},
		{"script hint", Input{Path: "/bundle", Hints: []discovery.Hint{discovery.HintScript}}, TypeAsset, AssetJavaScript, "script"},
		{"stylesheet hint", Input{Path: "/s", Hints: []discovery.Hint{discovery.HintStylesheet, discovery.HintLink}}, TypeAsset, AssetStylesheet, "stylesheet"},
		{"image ext", Input{Path: "/logo.PNG", Hints: link}, TypeAsset, AssetImage, ".png"},
		{"pdf ext", Input{Path: "/brochure.pdf", Hints: link}, TypeAsset, AssetDocument, ".pdf"},
		{"api path unverified", Input{Path: "/api/projects", Hints: link}, TypeAPI, "", "/api/ (not verified"},
		{"graphql path", Input{Path: "/graphql"}, TypeAPI, "", "/graphql/"},
		{"versioned path", Input{Path: "/v2/items"}, TypeAPI, "", "/v2/"},
		{"api-like word is not api", Input{Path: "/apidocs", Hints: link}, TypePage, "", "Linked"},
		{"sitemap xml not api", Input{Path: "/sitemap.xml", Hints: link}, TypePage, "", "Linked"},
		{"php page", Input{Path: "/index.php"}, TypePage, "", ".php"},
		{"link", Input{Path: "/about", Hints: link}, TypePage, "", "Linked from HTML"},
		{"entry redirect", Input{Path: "/", Hints: []discovery.Hint{discovery.HintEntry}, Status: 301}, TypePage, "", "start URL"},
		{"redirect target", Input{Path: "/home", Hints: []discovery.Hint{discovery.HintRedirect}}, TypePage, "", "redirect"},
		{"form", Input{Path: "/contact", Hints: []discovery.Hint{discovery.HintForm}}, TypeUnknown, "", "Form"},
		{"nothing", Input{Path: "/x"}, TypeUnknown, "", "Not enough"},
		{"archived html", Input{Path: "/about", Hints: []discovery.Hint{discovery.HintArchive}, ArchivedContentType: "text/html"}, TypePage, "", "Archived as text/html"},
		{"archived json", Input{Path: "/data", ArchivedContentType: "application/json"}, TypeAPI, "", "not requested"},
		{"live beats archive", Input{Path: "/x", Status: 200, ContentType: "application/json", ArchivedContentType: "text/html"}, TypeAPI, "", "Responded with"},
		{"archive unknown type", Input{Path: "/x", Hints: []discovery.Hint{discovery.HintArchive}, ArchivedContentType: "warc/revisit"}, TypeUnknown, "", "web archive"},
		{"sitemap", Input{Path: "/blog/post", Hints: []discovery.Hint{discovery.HintSitemap}}, TypePage, "", "sitemap"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.in)
			if got.Type != tt.typ || got.AssetKind != tt.kind {
				t.Errorf("got %s/%s, want %s/%s (evidence %q)", got.Type, got.AssetKind, tt.typ, tt.kind, got.Evidence)
			}
			if !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not contain %q", got.Evidence, tt.evidence)
			}
		})
	}
}
