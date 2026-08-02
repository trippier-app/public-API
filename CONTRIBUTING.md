# Contributing to public-API

Thanks for contributing! This repo holds the two public Trippier services:
`poi-api` (Go) and `itinerary-api` (Python). They run without accounts and
without tokens — that is a product decision, not an oversight.

## Getting set up

```sh
make setup     # .env from .env.example
make dev       # hot reload + Traefik, foreground
```

- poi-api → http://api.poi.trippier.localhost (also `:8080`)
- itinerary-api → http://api.ai.trippier.localhost (also `:8000`)
- Traefik dashboard → http://traefik.trippier.localhost (also `:8090`)

`make up` runs the same stack detached on the published ports, without Traefik.
Only one repo at a time can run Traefik locally — it binds port 80.

`make doctor` tells you what is missing if something does not start.

## Coding standards

Everything runs in throwaway containers, so no local Go or Python toolchain is
required:

```sh
make lint      # golangci-lint (poi) + ruff and mypy (itinerary)
make test      # go test -race (poi) + pytest (itinerary)
make tidy      # go mod tidy
```

CI runs the same four checks as separate jobs on every push and pull request.

## Environment variables

Every provider key is optional — the stack boots without any of them, and the
affected provider simply returns nothing. When you add one:

1. add it to `.env.example` under the right service heading,
2. pass it through `devops/docker-compose.yml` and `.prod.yml`,
3. give it a sane default in the service itself rather than making it required.

Production values live in the `DEPLOY_ENV` secret, never in the repo.

## Adding a provider to poi-api

Providers live behind a common interface in `internal/`. A new one should:

- degrade to an empty result rather than failing the whole request,
- respect `POI_PROVIDER_TIMEOUT`,
- go through the shared Redis cache with `POI_CACHE_TTL_SECONDS`,
- ship table-driven tests that do not hit the network.

## Workflow

1. Branch off an up-to-date `main`: `git checkout -b feat/my-feature`.
2. Commit with a conventional prefix: `feat:`, `fix:`, `docs:`, `chore:`, `ci:`.
3. Run `make lint` and `make test` before opening a PR.
4. Merging to `main` builds both images and rolls them out on the VPS — this
   stack also owns the shared edge Traefik that fronts every other Trippier
   stack, so treat changes to `devops/docker-compose.prod.yml` with care.
