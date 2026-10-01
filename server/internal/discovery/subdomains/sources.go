package subdomains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// The sources below are public lookup services that answer in about a
// second, where the certificate logs' own search (crt.sh) often takes a
// minute or fails. They are small, free services without published terms
// for automated use: each is asked once per domain (answers are cached),
// through the shared client's rate limit, and can be switched off.

// get performs one request to a provider and turns failures into errors
// that name it.
func get(ctx context.Context, f Fetcher, provider, u string) (*fetch.Response, error) {
	resp, err := f.Get(ctx, u)
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s did not respond in time", provider)
		}
		return nil, fmt.Errorf("%s request failed: %w", provider, err)
	case resp.StatusCode == 429:
		return nil, fmt.Errorf("%w: %s returned HTTP 429", ErrRateLimited, provider)
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("%s returned HTTP %d", provider, resp.StatusCode)
	case resp.Truncated:
		return nil, fmt.Errorf("%s response exceeded the size limit", provider)
	}
	return resp, nil
}

// Anubis queries AnubisDB, a public database of subdomains collected from
// several sources.
type Anubis struct {
	fetcher Fetcher
	// BaseURL is overridable for tests.
	BaseURL string
}

// NewAnubis creates an AnubisDB source.
func NewAnubis(f Fetcher) *Anubis { return &Anubis{fetcher: f, BaseURL: "https://anubisdb.com"} }

func (a *Anubis) Name() string                 { return "anubisdb" }
func (a *Anubis) Provenance() discovery.Source { return discovery.SourceDataset }

// Discover returns the subdomains AnubisDB lists for the domain.
func (a *Anubis) Discover(ctx context.Context, domain string) ([]string, error) {
	resp, err := get(ctx, a.fetcher, "AnubisDB", a.BaseURL+"/anubis/subdomains/"+url.PathEscape(domain))
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(resp.Body, &names); err != nil {
		return nil, fmt.Errorf("unexpected AnubisDB response: %w", err)
	}
	return names, nil
}

// thcLimit is how many names are asked of ip.thc.org in its one request.
const thcLimit = 10000

// THC queries ip.thc.org, a public database of hostnames seen in DNS.
type THC struct {
	fetcher Fetcher
	// BaseURL is overridable for tests.
	BaseURL string
}

// NewTHC creates an ip.thc.org source.
func NewTHC(f Fetcher) *THC { return &THC{fetcher: f, BaseURL: "https://ip.thc.org"} }

func (t *THC) Name() string                 { return "ip.thc.org" }
func (t *THC) Provenance() discovery.Source { return discovery.SourceDataset }

// Discover returns the subdomains ip.thc.org lists for the domain. Its
// download endpoint answers with one name per line under a header line.
func (t *THC) Discover(ctx context.Context, domain string) ([]string, error) {
	q := url.Values{"domain": {domain}, "limit": {fmt.Sprint(thcLimit)}}
	resp, err := get(ctx, t.fetcher, "ip.thc.org", t.BaseURL+"/api/v1/subdomains/download?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var names []string
	for i, line := range strings.Split(string(resp.Body), "\n") {
		// A line is a name, or a name followed by further columns.
		name, _, _ := strings.Cut(strings.TrimSpace(line), ",")
		if name == "" || (i == 0 && !strings.Contains(name, ".")) {
			continue // blank, or the header
		}
		names = append(names, name)
	}
	return names, nil
}

// ShodanCT queries Shodan's Certificate Transparency search, a third view
// of the certificate logs.
type ShodanCT struct {
	fetcher Fetcher
	// BaseURL is overridable for tests.
	BaseURL string
}

// NewShodanCT creates a Shodan CT source.
func NewShodanCT(f Fetcher) *ShodanCT {
	return &ShodanCT{fetcher: f, BaseURL: "https://ctl.shodan.io"}
}

func (s *ShodanCT) Name() string                 { return "shodan-ct" }
func (s *ShodanCT) Provenance() discovery.Source { return discovery.SourceCT }

type shodanCertificate struct {
	SubjectCN string   `json:"subject_cn"`
	DNSNames  []string `json:"san_dns_names"`
}

// Discover returns every name on the certificates Shodan lists for the
// domain.
func (s *ShodanCT) Discover(ctx context.Context, domain string) ([]string, error) {
	resp, err := get(ctx, s.fetcher, "Shodan CT", s.BaseURL+"/api/v1/domain/"+url.PathEscape(domain))
	if err != nil {
		return nil, err
	}
	var certs []shodanCertificate
	if err := json.Unmarshal(resp.Body, &certs); err != nil {
		return nil, fmt.Errorf("unexpected Shodan CT response: %w", err)
	}
	var names []string
	for _, c := range certs {
		names = append(names, c.SubjectCN)
		names = append(names, c.DNSNames...)
	}
	return names, nil
}
