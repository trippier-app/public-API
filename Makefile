ENGINE   ?= docker
COMPOSE  := $(ENGINE) compose

COMPOSE_HOT := $(COMPOSE) --project-directory $(CURDIR) -f devops/docker-compose.yml -f devops/docker-compose.dev.yml
COMPOSE_ALL := $(COMPOSE_HOT) -f devops/docker-compose.traefik.yml -f devops/docker-compose.edge.yml
COMPOSE_DEV  = $(COMPOSE_HOT) -f devops/docker-compose.traefik.yml $(EDGE_FILE)

PROJECT    := trippier-public-api
EDGE_NET   := trippier-edge
TRAEFIK_CT := trippier-traefik
EDGE_FILE   = $(if $(shell $(ENGINE) ps -q -f 'name=^$(TRAEFIK_CT)$$' -f status=running 2>/dev/null),,-f devops/docker-compose.edge.yml)

REGISTRY ?= ghcr.io
OWNER    ?= trippier-app
TAG      ?= latest

POI_IMAGE       = $(REGISTRY)/$(OWNER)/poi-api:$(TAG)
ITINERARY_IMAGE = $(REGISTRY)/$(OWNER)/itinerary-api:$(TAG)

UID_GID := $(shell id -u):$(shell id -g)
CACHE   := $(HOME)/.cache/trippier

DRUN    := $(ENGINE) run --rm -u $(UID_GID)
DRUN_GO := $(DRUN) \
	-e GOCACHE=/cache/go-build \
	-e GOPATH=/cache/go \
	-e GOLANGCI_LINT_CACHE=/cache/golangci \
	-v $(CACHE)/go-build:/cache/go-build:z \
	-v $(CACHE)/go:/cache/go:z \
	-v $(CACHE)/golangci:/cache/golangci:z
DRUN_PY := $(ENGINE) run --rm

SERVICE ?=

ifndef NO_COLOR
GRN  := \033[1;32m
CYAN := \033[1;36m
BOLD := \033[1m
DIM  := \033[2m
RST  := \033[0m
endif

step = @printf "$(GRN)▶$(RST) %s\n"

.PHONY: help setup doctor check-ports \
	dev dev-stop logs up down \
	build push \
	test test-go-poi test-python \
	lint lint-go-poi lint-python \
	tidy clean

.DEFAULT_GOAL := help

#################################### Setup #####################################

setup:
	@if [ -f .env ]; then echo ".env already exists, nothing to do."; else \
		cp .env.example .env; echo "Created .env"; \
	fi

doctor:
	@printf "$(BOLD)public-API doctor$(RST)\n"
	@command -v $(ENGINE) >/dev/null 2>&1 \
		&& printf "  [ok] $(ENGINE): %s\n" "$$($(ENGINE) --version | head -1)" \
		|| printf "  [!!] $(ENGINE) not found\n"
	@$(COMPOSE) version >/dev/null 2>&1 \
		&& printf "  [ok] '$(ENGINE) compose' available\n" \
		|| printf "  [!!] '$(ENGINE) compose' not available\n"
	@[ -f .env ] && printf "  [ok] .env present\n" || printf "  [!!] .env missing, run 'make setup'\n"
	@$(COMPOSE_ALL) config -q >/dev/null 2>&1 \
		&& printf "  [ok] compose files are valid\n" \
		|| printf "  [!!] compose files have errors\n"
	@$(MAKE) --no-print-directory check-ports >/dev/null 2>&1 \
		&& printf "  [ok] published ports are free\n" \
		|| printf "  [!!] port conflict, run 'make check-ports'\n"

check-ports:
	@busy=""; \
	for p in $$($(COMPOSE_HOT) config 2>/dev/null | sed -n 's/.*published: "\([0-9]*\)".*/\1/p' | sort -u); do \
		owner=$$($(ENGINE) ps --format '{{.Label "com.docker.compose.project"}} {{.Ports}}' 2>/dev/null | grep ":$$p->" | head -1 | cut -d' ' -f1); \
		if [ -n "$$owner" ] && [ "$$owner" != "$(PROJECT)" ]; then busy="$$busy $$p($$owner)"; \
		elif [ -z "$$owner" ] && ss -ltnH "sport = :$$p" 2>/dev/null | grep -q .; then busy="$$busy $$p(host)"; fi; \
	done; \
	[ -z "$$busy" ] || { printf "  [!!] port already taken:$$busy\n  see PORTS.md at the trippier-org root\n"; exit 1; }

################################## Development #################################

up: check-ports
	$(step) "Starting stack (hot reload, detached, no Traefik)…"
	@$(COMPOSE_HOT) up -d --build

down:
	$(step) "Stopping stack…"
	@$(COMPOSE_ALL) down

dev: check-ports
	$(step) "Starting dev stack (hot reload + Traefik on *.trippier.localhost:8100)…"
	@$(ENGINE) network inspect $(EDGE_NET) >/dev/null 2>&1 || $(ENGINE) network create $(EDGE_NET) >/dev/null
	@[ -n "$$($(ENGINE) ps -q -f 'name=^$(TRAEFIK_CT)$$' -f status=running)" ] || $(ENGINE) rm -f $(TRAEFIK_CT) >/dev/null 2>&1 || true
	@$(COMPOSE_DEV) up --build

dev-stop:
	$(step) "Stopping dev stack (removing volumes)…"
	@$(COMPOSE_ALL) down -v

logs:
	$(step) "Following logs$(if $(SERVICE), for $(SERVICE),)…"
	@$(COMPOSE_ALL) logs -f $(SERVICE)

############################ Build & publish images ############################

build:
	$(step) "Building poi-api image ($(POI_IMAGE))…"
	@$(ENGINE) build -t $(POI_IMAGE)       ./poi-api
	$(step) "Building itinerary-api image ($(ITINERARY_IMAGE))…"
	@$(ENGINE) build -t $(ITINERARY_IMAGE) ./itinerary-api

push: build
	$(step) "Pushing poi-api image…"
	@$(ENGINE) push $(POI_IMAGE)
	$(step) "Pushing itinerary-api image…"
	@$(ENGINE) push $(ITINERARY_IMAGE)

########### Tests (throwaway containers, no local toolchain needed) ###########

test-go-poi:
	$(step) "Testing poi-api (go test -race)…"
	@$(DRUN_GO) -v $(CURDIR)/poi-api:/app:z -w /app golang:1.25 go test -race ./...

test-python:
	$(step) "Testing itinerary-api (pytest)…"
	@$(DRUN_PY) -v $(CURDIR)/itinerary-api:/app:z -w /app python:3.14-slim \
		sh -c "pip install -q -r requirements-dev.txt && pytest --tb=short"

test: test-go-poi test-python

##################################### Lint #####################################

lint-go-poi:
	$(step) "Linting poi-api (golangci-lint)…"
	@$(DRUN_GO) -v $(CURDIR)/poi-api:/app:z -w /app golangci/golangci-lint:v2.12.2 golangci-lint run --timeout 5m

lint-python:
	$(step) "Linting itinerary-api (ruff + mypy)…"
	@$(DRUN_PY) -v $(CURDIR)/itinerary-api:/app:z -w /app python:3.14-slim \
		sh -c "pip install -q -r requirements-dev.txt && ruff check . && mypy app"

lint: lint-go-poi lint-python

##################################### Misc #####################################

tidy:
	$(step) "Tidying poi-api go.mod…"
	@$(DRUN_GO) -v $(CURDIR)/poi-api:/app:z -w /app golang:1.25-alpine go mod tidy

clean:
	$(step) "Tearing down the stack (with volumes)…"
	@-$(COMPOSE_ALL) down -v --remove-orphans

help:
	@printf "$(BOLD)Usage:$(RST) make $(CYAN)<target>$(RST)  [ENGINE=podman] [OWNER=… TAG=…]\n"
	@printf "\n$(BOLD)Development$(RST)\n"
	@printf "  $(CYAN)setup$(RST)\t\t Create .env from .env.example\n"
	@printf "  $(CYAN)doctor$(RST)\t Check the machine is ready to run the stack\n"
	@printf "  $(CYAN)up$(RST) / $(CYAN)down$(RST)\t Hot reload on published ports (detached, no Traefik)\n"
	@printf "  $(CYAN)dev$(RST)\t\t Hot reload + shared Traefik on *.trippier.localhost:8100 (Ctrl-C to stop)\n"
	@printf "  $(CYAN)dev-stop$(RST)\t Stop the dev stack (removes volumes)\n"
	@printf "  $(CYAN)logs$(RST)\t\t Follow logs (make logs SERVICE=poi-api)\n"
	@printf "\n$(BOLD)Images$(RST)\n"
	@printf "  $(CYAN)build$(RST) / $(CYAN)push$(RST)\t Build / publish poi-api + itinerary-api images\n"
	@printf "\n$(BOLD)Quality$(RST)\n"
	@printf "  $(CYAN)lint$(RST)\t\t Lint both services (Go poi + Python itinerary)\n"
	@printf "  $(CYAN)test$(RST)\t\t Test both services in throwaway containers\n"
	@printf "  $(CYAN)tidy$(RST)\t\t go mod tidy the Go service\n"
	@printf "  $(CYAN)clean$(RST)\t\t Tear down the stack with volumes\n"
	@printf "\n$(DIM)Host ports are listed in PORTS.md at the trippier-org root.$(RST)\n"
	@printf "$(DIM)Per-service: lint-go-poi/-python, test-go-poi/-python.$(RST)\n"
	@printf "$(DIM)Swap the engine with ENGINE=podman. Override images with OWNER= TAG=.$(RST)\n"
