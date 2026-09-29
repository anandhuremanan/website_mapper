package htmlcrawl

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// page builds an HTML document of roughly size bytes with a link every
// ~200 bytes, similar to a link-heavy listing page.
func benchPage(size int) []byte {
	var b strings.Builder
	b.WriteString("<html><head><title>Bench</title></head><body>")
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, `<p>Some paragraph text %d <a href="/section/%d/article-%d">link</a> more text here.</p>`, i, i%50, i)
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

func BenchmarkExtract(b *testing.B) {
	base, _ := url.Parse("https://example.com/")
	for _, size := range []int{100 << 10, 2 << 20} {
		body := benchPage(size)
		b.Run(fmt.Sprintf("%dKB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				_ = extract(base, body)
			}
		})
	}
}
