# Security Policy

## Supported versions

Security fixes land on `main` and the most recent tagged release.

## Reporting a vulnerability

Do not open a public issue for security problems. Report privately via GitHub
Security Advisories:

https://github.com/trippier-app/public-API/security/advisories/new

Or email the maintainer at ulyssemercadal@kakao.com.

Please include:

- a description of the issue and its impact,
- steps to reproduce (a proof of concept if you have one),
- the affected service — `poi-api` or `itinerary-api`.

We aim to acknowledge reports within 72 hours and to ship a fix or mitigation
for confirmed issues as fast as is practical.

## Scope

In scope: SSRF or request forgery through the upstream provider calls, injection
in any parameter forwarded to a provider, cache poisoning of the shared Redis,
leakage of the provider keys (GeoNames, Ticketmaster, Eventbrite), and requests
that let a caller exhaust the host's memory or file descriptors.

Also in scope, and treated seriously: anything in this stack that could affect
the other Trippier stacks, since it owns the edge Traefik and its Let's Encrypt
storage.

Out of scope: the absence of authentication and of rate limiting. Both services
run with auth disabled on purpose — the API is public by design, so "no login
required" and "an anonymous caller can query it" are not vulnerabilities.
