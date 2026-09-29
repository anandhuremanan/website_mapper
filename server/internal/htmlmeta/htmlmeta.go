// Package htmlmeta extracts small pieces of metadata from HTML documents.
package htmlmeta

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// Title returns the whitespace-collapsed text of the first <title>, or "".
func Title(body []byte) string {
	z := html.NewTokenizer(bytes.NewReader(body))
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				inTitle = true
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); string(name) == "title" {
				return ""
			}
		case html.TextToken:
			if inTitle {
				return strings.Join(strings.Fields(string(z.Text())), " ")
			}
		}
	}
}
