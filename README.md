# public-API

The public Trippier travel API. Two services, no accounts, no tokens.

| Service         | Language | Port | What it does                              |
| --------------- | -------- | ---- | ----------------------------------------- |
| `poi-api`       | Go       | 8080 | Points of interest search and event feeds |
| `itinerary-api` | Python   | 8000 | Itinerary generation (calls `poi-api`)    |

Both run with auth **disabled** (`POI_AUTH_DISABLED` / `AUTH_DISABLED`), so the
stack is genuinely public: no login, no rate limiting.

## Quickstart

```sh
make setup     # create .env from .env.example
make dev       # hot reload + Traefik (foreground, Ctrl-C to stop)
```

`make dev` puts a Traefik proxy in front of the hot-reload stack. The URLs use
the `*.localhost` TLD, which resolves to 127.0.0.1 with no /etc/hosts edits:

- poi-api        → http://api.poi.trippier.localhost  (also http://localhost:8080)
- itinerary-api  → http://api.ai.trippier.localhost   (also http://localhost:8000)
- Traefik board  → http://traefik.trippier.localhost  (also http://localhost:8090)

`make up` runs the same hot-reload stack detached on the published ports, with
no Traefik:

```sh
make up        # start detached, hot reload on :8080 / :8000
make down      # stop
```

## Configuration

Copy `.env.example` to `.env` (via `make setup`) and fill in the optional
provider keys (GeoNames, Ticketmaster, Eventbrite). Everything has a default, so
no key is required to boot.

## Quality

```sh
make test      # go test (poi) + pytest (itinerary), in throwaway containers
make lint      # golangci-lint (poi) + ruff/mypy (itinerary)
```

Everything runs in containers, so you need no local Go or Python toolchain. Swap
the engine with `ENGINE=podman`. Run `make help` for the full target list.

## Images

```sh
make build     # build ghcr.io/trippier-app/{poi-api,itinerary-api}:latest
make push      # publish them (override with OWNER= TAG=)
```
