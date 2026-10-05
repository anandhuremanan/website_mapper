package subdomains

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"websitemapper/internal/discovery"
	"websitemapper/internal/fetch"
)

// Fetcher performs a single GET request.
type Fetcher interface {
	Get(ctx context.Context, url string) (*fetch.Response, error)
}

// CRTSh queries the crt.sh Certificate Transparency search.
type CRTSh struct {
	fetcher Fetcher
	// BaseURL is overridable for tests.
	BaseURL string
	// RetryDelay is the wait before retrying a failed request.
	RetryDelay time.Duration
	// DB, if set, is asked first: crt.sh's database is often available
	// when its website is not. The website is the fallback.
	DB NameLister
}

// NewCRTSh creates a crt.sh source. The fetcher should allow a long timeout
// and a large body limit: crt.sh is slow and responses for busy domains are
// several megabytes.
func NewCRTSh(f Fetcher) *CRTSh {
	return &CRTSh{fetcher: f, BaseURL: "https://crt.sh", RetryDelay: 3 * time.Second}
}

func (c *CRTSh) Name() string                 { return "crt.sh" }
func (c *CRTSh) Provenance() discovery.Source { return discovery.SourceCT }

// Discover returns every name on certificates logged for the domain and
// its subdomains: from crt.sh's database if one is set and answers, else
// from its website. The website often fails transiently under load, so a
// failed request there is retried twice with increasing delays.
func (c *CRTSh) Discover(ctx context.Context, domain string) ([]string, error) {
	if c.DB == nil {
		return c.website(ctx, domain)
	}
	names, dbErr := c.DB.Names(ctx, domain)
	if dbErr == nil {
		return names, nil
	}
	if ctx.Err() != nil {
		return nil, dbErr
	}
	names, webErr := c.website(ctx, domain)
	if webErr != nil {
		if errors.Is(webErr, ErrRateLimited) {
			return nil, fmt.Errorf("%w (and %s)", webErr, dbErr)
		}
		return nil, fmt.Errorf("%s; %s", dbErr, webErr)
	}
	return names, nil
}

// website asks crt.sh's web interface.
func (c *CRTSh) website(ctx context.Context, domain string) ([]string, error) {
	u := c.BaseURL + "/?q=" + url.QueryEscape("%."+domain) + "&output=json"
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * c.RetryDelay):
			}
		}
		resp, err := c.fetcher.Get(ctx, u)
		switch {
		case err != nil:
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("crt.sh did not respond in time")
			}
			lastErr = fmt.Errorf("crt.sh request failed: %w", err)
		case resp.StatusCode == 200:
			if resp.Truncated {
				return nil, fmt.Errorf("crt.sh response exceeded the size limit")
			}
			return ParseCRTSh(resp.Body)
		case resp.StatusCode == 429:
			lastErr = fmt.Errorf("%w: crt.sh returned HTTP 429", ErrRateLimited)
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("crt.sh returned HTTP %d", resp.StatusCode)
		default:
			return nil, fmt.Errorf("crt.sh returned HTTP %d", resp.StatusCode)
		}
	}
	return nil, lastErr
}

type crtshEntry struct {
	NameValue  string `json:"name_value"`
	CommonName string `json:"common_name"`
}

// ParseCRTSh extracts raw names from a crt.sh JSON response. name_value
// holds one or more newline-separated names. Names are returned as found;
// Filter normalizes and scopes them.
func ParseCRTSh(body []byte) ([]string, error) {
	var entries []crtshEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("unexpected crt.sh response: %w", err)
	}
	seen := make(map[string]bool)
	var names []string
	add := func(n string) {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, e := range entries {
		for _, n := range strings.Split(e.NameValue, "\n") {
			add(n)
		}
		add(e.CommonName)
	}
	return names, nil
}
