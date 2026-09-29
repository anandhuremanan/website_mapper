package normalize

import (
	"net/url"
	"strings"
)

// Redacted replaces the values of sensitive query parameters.
const Redacted = "REDACTED"

// sensitiveParams are query parameters whose values are credentials or
// session identifiers. Matching is case-insensitive.
var sensitiveParams = map[string]bool{
	"password": true, "passwd": true, "pwd": true, "pass": true,
	"token": true, "access_token": true, "refresh_token": true, "id_token": true, "auth_token": true,
	"auth": true, "authorization": true, "code": true,
	"api_key": true, "apikey": true, "key": true, "secret": true, "client_secret": true,
	"session": true, "sessionid": true, "session_id": true, "sid": true, "jsessionid": true, "phpsessid": true,
	"signature": true, "sig": true,
	"x-amz-signature": true, "x-amz-credential": true, "x-amz-security-token": true,
	"x-goog-signature": true, "x-goog-credential": true,
}

// RedactURL returns a copy of u without user info and with the values of
// sensitive query parameters replaced, so that stored results never
// contain credentials that happened to appear in links.
func RedactURL(u *url.URL) *url.URL {
	c := *u
	c.User = nil
	if c.RawQuery == "" {
		return &c
	}
	values, err := url.ParseQuery(c.RawQuery)
	if err != nil {
		c.RawQuery = Redacted
		return &c
	}
	changed := false
	for k, vs := range values {
		if sensitiveParams[strings.ToLower(k)] {
			for i := range vs {
				vs[i] = Redacted
			}
			changed = true
		}
	}
	if changed {
		c.RawQuery = values.Encode()
	}
	return &c
}

// RedactString applies RedactURL to a URL string. Unparseable input is
// returned unchanged.
func RedactString(raw string) string {
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return RedactURL(u).String()
}
