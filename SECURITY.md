# Security policy

## Reporting a vulnerability

Please report security problems privately, not in public issues:

- use GitHub's **[Report a vulnerability](https://github.com/anandhuremanan/website_mapper/security/advisories/new)**
  form (Security tab → Advisories), or
- contact the maintainer through [imanandhu.in](https://imanandhu.in).

Include what you found, how to reproduce it and what an attacker could do
with it. You will get a reply as soon as possible; please allow reasonable
time for a fix before disclosing publicly.

## What counts as a vulnerability

Web Scanner makes outbound requests on behalf of users, so issues that
make it misbehave toward other systems are the most important, for example:

- reaching private, loopback, link-local or cloud metadata addresses
  (server-side request forgery), including through redirects or DNS
  rebinding;
- making requests outside the scanned domain's scope, or requests other than
  `GET`;
- bypassing rate limits, request budgets or bandwidth limits;
- crashing the server or exhausting its memory with a crafted site, sitemap
  or API request;
- reading or cancelling another user's scan in a way the documentation does
  not describe.

The address checks are described in
[ARCHITECTURE.md](ARCHITECTURE.md#outbound-requests-the-fetch-client).

## Supported versions

Only the latest code on the `main` branch is supported.

## Reporting abuse

If a Web Scanner instance is sending unwanted traffic to your site,
contact whoever operates that instance. The scanner identifies itself with
the `User-Agent` configured by its operator (`SCAN_USER_AGENT`). For the
instance run by the maintainer, use the contact above.
