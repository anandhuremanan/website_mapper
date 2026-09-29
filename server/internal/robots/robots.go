// Package robots parses robots.txt files (RFC 9309) and answers whether a
// crawler may fetch a path.
//
// Only Allow/Disallow rules are used. Disallow entries describe what not to
// fetch; they are never treated as evidence that a route exists.
package robots

import (
	"bufio"
	"bytes"
	"regexp"
	"strings"
)

// Rules are the rules that apply to one user agent.
type Rules struct {
	rules []rule
}

type rule struct {
	allow   bool
	pattern string
	re      *regexp.Regexp
}

// AllowAll permits everything; used when robots.txt is missing or unreadable.
var AllowAll = &Rules{}

// Parse returns the rules in body that apply to agent, a product token such
// as "WebsiteMapperBot" (matched case-insensitively). Groups naming the agent
// take precedence over the "*" group; several matching groups are merged.
func Parse(body []byte, agent string) *Rules {
	agent = strings.ToLower(agent)
	var specific, wildcard []rule
	var hasSpecific bool

	// A group is one or more user-agent lines followed by rules.
	var groupAgents []string
	inRules := false

	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 512*1024)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "user-agent":
			if inRules {
				groupAgents, inRules = nil, false
			}
			ua := strings.ToLower(value)
			if i := strings.IndexByte(ua, '/'); i >= 0 {
				ua = ua[:i] // "WebsiteMapperBot/0.1" names the same product
			}
			groupAgents = append(groupAgents, strings.TrimSpace(ua))
		case "allow", "disallow":
			inRules = true
			if len(groupAgents) == 0 {
				continue // rule outside any group
			}
			if value == "" {
				continue // an empty Disallow allows everything
			}
			r := rule{allow: key == "allow", pattern: value, re: compile(value)}
			for _, a := range groupAgents {
				switch a {
				case agent:
					specific, hasSpecific = append(specific, r), true
				case "*":
					wildcard = append(wildcard, r)
				}
			}
		default:
			// Sitemap, Crawl-delay and unknown keys end neither the group
			// nor affect access.
		}
	}
	if hasSpecific {
		return &Rules{rules: specific}
	}
	return &Rules{rules: wildcard}
}

// Allowed reports whether path (with its query, e.g. "/search?q=1") may be
// fetched. The longest matching rule wins; on a tie, Allow wins.
func (r *Rules) Allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	if path == "/robots.txt" {
		return true
	}
	best, allowed := -1, true
	for _, ru := range r.rules {
		if !ru.re.MatchString(path) {
			continue
		}
		n := len(ru.pattern)
		if n > best || (n == best && ru.allow) {
			best, allowed = n, ru.allow
		}
	}
	return allowed
}

// compile turns a robots.txt path pattern into an anchored regexp:
// "*" matches any sequence and a trailing "$" anchors the end.
func compile(pattern string) *regexp.Regexp {
	anchored := strings.HasSuffix(pattern, "$")
	pattern = strings.TrimSuffix(pattern, "$")
	var b strings.Builder
	b.WriteString("^")
	for i, part := range strings.Split(pattern, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(part))
	}
	if anchored {
		b.WriteString("$")
	}
	return regexp.MustCompile(b.String())
}
