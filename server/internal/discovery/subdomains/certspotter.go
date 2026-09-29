package subdomains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"websitemapper/internal/discovery"
)

// certSpotterMaxPages bounds pagination. Unauthenticated use is limited to
// a few requests per hour, and each page is one request.
const certSpotterMaxPages = 3

// CertSpotter queries SSLMate's Cert Spotter API, a second Certificate
// Transparency source. It only returns certificates that have not expired,
// which complements crt.sh's full history.
type CertSpotter struct {
	fetcher Fetcher
	// BaseURL is overridable for tests.
	BaseURL string
}

// NewCertSpotter creates a Cert Spotter source.
func NewCertSpotter(f Fetcher) *CertSpotter {
	return &CertSpotter{fetcher: f, BaseURL: "https://api.certspotter.com"}
}

func (c *CertSpotter) Name() string                 { return "certspotter" }
func (c *CertSpotter) Provenance() discovery.Source { return discovery.SourceCT }

type certSpotterIssuance struct {
	ID       string   `json:"id"`
	DNSNames []string `json:"dns_names"`
}

// Discover returns DNS names from current certificates for the domain and
// its subdomains, following pagination up to certSpotterMaxPages.
func (c *CertSpotter) Discover(ctx context.Context, domain string) ([]string, error) {
	q := url.Values{
		"domain":             {domain},
		"include_subdomains": {"true"},
		"expand":             {"dns_names"},
	}
	var names []string
	for page := 0; page < certSpotterMaxPages; page++ {
		resp, err := c.fetcher.Get(ctx, c.BaseURL+"/v1/issuances?"+q.Encode())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return names, fmt.Errorf("Cert Spotter did not respond in time")
			}
			return names, fmt.Errorf("Cert Spotter request failed: %w", err)
		}
		switch {
		case resp.StatusCode == 429 && page > 0:
			return names, nil // rate limited mid-way; keep what we have
		case resp.StatusCode != 200:
			return nil, fmt.Errorf("Cert Spotter returned HTTP %d", resp.StatusCode)
		}
		var issuances []certSpotterIssuance
		if err := json.Unmarshal(resp.Body, &issuances); err != nil {
			return nil, fmt.Errorf("unexpected Cert Spotter response: %w", err)
		}
		for _, is := range issuances {
			names = append(names, is.DNSNames...)
		}
		if len(issuances) == 0 {
			break
		}
		q.Set("after", issuances[len(issuances)-1].ID)
	}
	return names, nil
}
