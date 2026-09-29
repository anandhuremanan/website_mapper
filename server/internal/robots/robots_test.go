package robots

import "testing"

func TestParseWildcardGroup(t *testing.T) {
	r := Parse([]byte(`
# comment
User-agent: *
Disallow: /admin
Disallow: /private/   # trailing comment
Allow: /admin/public
Disallow:

Sitemap: https://example.com/sitemap.xml
`), "WebsiteMapperBot")

	for path, want := range map[string]bool{
		"/":               true,
		"/about":          true,
		"/admin":          false,
		"/admin/users":    false,
		"/administrator":  false, // prefix match, as specified
		"/admin/public":   true,  // longer Allow wins
		"/admin/public/x": true,
		"/private/":       false,
		"/private":        true,
		"/robots.txt":     true,
		"/search?q=admin": true,
	} {
		if got := r.Allowed(path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestParseSpecificGroupWins(t *testing.T) {
	body := []byte(`
User-agent: *
Disallow: /

User-agent: websitemapperbot/0.1
User-agent: OtherBot
Disallow: /drafts
`)
	r := Parse(body, "WebsiteMapperBot")
	if !r.Allowed("/blog") || r.Allowed("/drafts/1") {
		t.Error("specific group should replace the * group")
	}
	// A bot without a specific group falls back to *.
	if Parse(body, "SomeoneElse").Allowed("/blog") {
		t.Error("* group should apply to other agents")
	}
}

func TestParseMergesGroupsAndHandlesWildcards(t *testing.T) {
	r := Parse([]byte(`
User-agent: WebsiteMapperBot
Disallow: /*.pdf$
Disallow: /tmp*/cache

User-agent: WebsiteMapperBot
Disallow: /search?
Allow: /search?page=
`), "WebsiteMapperBot")
	for path, want := range map[string]bool{
		"/files/a.pdf":     false,
		"/files/a.pdf?x=1": true, // $ anchors the end
		"/tmp1/cache/x":    false,
		"/tmp/other":       true,
		"/search?q=x":      false,
		"/search?page=2":   true, // longer Allow wins
		"/searching":       true,
	} {
		if got := r.Allowed(path); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestTieGoesToAllow(t *testing.T) {
	r := Parse([]byte("User-agent: *\nDisallow: /page\nAllow: /page\n"), "x")
	if !r.Allowed("/page") {
		t.Error("equal-length Allow should win")
	}
}

func TestDisallowAllAndEmptyFiles(t *testing.T) {
	if Parse([]byte("User-agent: *\nDisallow: /\n"), "x").Allowed("/anything") {
		t.Error("Disallow: / should block everything")
	}
	for _, body := range []string{"", "garbage without colons", "Disallow: /x\n"} {
		if !Parse([]byte(body), "x").Allowed("/x") {
			t.Errorf("%q should allow everything", body)
		}
	}
	if !AllowAll.Allowed("/admin") {
		t.Error("AllowAll")
	}
}
