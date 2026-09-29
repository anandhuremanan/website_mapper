package normalize

import "testing"

func TestRedactString(t *testing.T) {
	for in, want := range map[string]string{
		"https://example.com/a":                            "https://example.com/a",
		"https://example.com/a?page=2":                     "https://example.com/a?page=2",
		"https://example.com/reset?token=abc123&page=2":    "https://example.com/reset?page=2&token=REDACTED",
		"https://example.com/x?API_KEY=k&Password=p":       "https://example.com/x?API_KEY=REDACTED&Password=REDACTED",
		"https://user:pw@example.com/a":                    "https://example.com/a",
		"https://b.s3.amazonaws.com/f?X-Amz-Signature=abc": "https://b.s3.amazonaws.com/f?X-Amz-Signature=REDACTED",
		"": "",
	} {
		if got := RedactString(in); got != want {
			t.Errorf("RedactString(%q) = %q, want %q", in, got, want)
		}
	}
}
