# =============================================================================
# Makefile — every command you need, discoverable with `make help`.
#
# WHY A MAKEFILE: the alternative is a README full of long commands that go
# stale, and a new reader who cannot tell which ones still work. `make test`
# is one word, and if it breaks, it breaks visibly for everyone.
#
# Run `make help` first.
# =============================================================================

# .DEFAULT_GOAL means a bare `make` prints help instead of silently building
# the first target it happens to find.
.DEFAULT_GOAL := help

# .PHONY tells make these targets are COMMANDS, not files to produce. Without
# it, `make test` would do nothing if a file named "test" ever existed — a
# genuinely baffling failure.
.PHONY: help build test test-go test-ai test-race test-cover lint fmt vet \
        run run-nodocker docker-up docker-down docker-logs docker-rebuild \
        e2e smoke demo migrate psql redis-cli clean tidy check ci \
        load-test watch-logs stats

# Use bash with strict flags, so a failing command inside a recipe actually
# fails the target instead of being ignored.
SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

GO          ?= go
PYTHON      ?= python3
COMPOSE     ?= docker compose
VERSION     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
SERVICES    := orderd inventoryd paymentd legacyd

# Colours make `make help` scannable. tput degrades gracefully when the
# terminal does not support colour, so this is safe in CI logs.
BOLD  := $(shell tput bold 2>/dev/null || true)
RESET := $(shell tput sgr0 2>/dev/null || true)
CYAN  := $(shell tput setaf 6 2>/dev/null || true)

## help: show this message
help:
	@echo ""
	@echo "$(BOLD)Order Processing System$(RESET)  (version $(VERSION))"
	@echo ""
	@echo "$(BOLD)Getting started$(RESET)"
	@echo "  $(CYAN)make docker-up$(RESET)      start everything (Postgres, Redis, 4 Go services, 1 Python service)"
	@echo "  $(CYAN)make e2e$(RESET)            run the full end-to-end test against the running stack"
	@echo "  $(CYAN)make demo$(RESET)           walk through every feature with real curl commands"
	@echo "  $(CYAN)make docker-down$(RESET)    stop everything"
	@echo ""
	@echo "$(BOLD)No Docker available?$(RESET)"
	@echo "  $(CYAN)make run-nodocker$(RESET)   run the order service alone, in-memory (no DB, no Redis)"
	@echo ""
	@echo "$(BOLD)Testing$(RESET)"
	@echo "  $(CYAN)make test$(RESET)           Go + Python unit and integration tests"
	@echo "  $(CYAN)make test-race$(RESET)      the same, with the race detector (slower, finds concurrency bugs)"
	@echo "  $(CYAN)make test-cover$(RESET)     produce and open an HTML coverage report"
	@echo "  $(CYAN)make check$(RESET)          fmt + vet + test  (run this before committing)"
	@echo ""
	@echo "$(BOLD)Inspecting a running system$(RESET)"
	@echo "  $(CYAN)make docker-logs$(RESET)    tail every service's logs"
	@echo "  $(CYAN)make watch-logs$(RESET)     tail only the order service"
	@echo "  $(CYAN)make stats$(RESET)          show order stats, cache hit ratio and migration progress"
	@echo "  $(CYAN)make psql$(RESET)           open a SQL shell on the orders database"
	@echo "  $(CYAN)make redis-cli$(RESET)      open a Redis shell (try: KEYS 'order:*')"
	@echo ""
	@echo "$(BOLD)Everything else$(RESET)"
	@grep -E '^## [a-z-]+:' $(MAKEFILE_LIST) | sed 's/## /  /' | sort
	@echo ""
	@echo "$(BOLD)Where things live$(RESET)"
	@echo "  API docs (Swagger UI)   http://localhost:8080/docs"
	@echo "  Order service           http://localhost:8080"
	@echo "  Assistant (chatbot)     http://localhost:8090"
	@echo "  Start reading at        cmd/orderd/main.go, then docs/CODE_WALKTHROUGH.md"
	@echo ""

# -----------------------------------------------------------------------------
# Build
# -----------------------------------------------------------------------------

## build: compile all four Go binaries into ./bin
build:
	@mkdir -p bin
	@for svc in $(SERVICES); do \
		echo "building $$svc"; \
		CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o bin/$$svc ./cmd/$$svc; \
	done
	@echo "built: $$(ls bin/ | tr '\n' ' ')"

## tidy: sync go.mod/go.sum with the imports actually used
tidy:
	$(GO) mod tidy

# -----------------------------------------------------------------------------
# Tests
# -----------------------------------------------------------------------------

## test: run all Go and Python tests
test: test-go test-ai

## test-go: run the Go test suite
test-go:
	@echo "=== Go tests ==="
	$(GO) test ./... -count=1
	@echo ""
	@echo "NOTE: the Postgres integration tests SKIP unless a database is reachable."
	@echo "      Run 'make docker-up' first to include them."

## test-ai: run the Python RAG test suite
test-ai:
	@echo "=== Python tests (RAG pipeline) ==="
	@cd ai && $(PYTHON) -m unittest discover -s tests -t . -v 2>&1 | tail -20

## test-race: run the Go tests under the race detector
test-race:
	@echo "=== Go tests with -race ==="
	@echo "The race detector instruments every memory access, so this is ~10x"
	@echo "slower — but it is the ONLY reliable way to find a data race."
	$(GO) test -race ./... -count=1

## test-cover: produce an HTML coverage report and open it
test-cover:
	$(GO) test ./... -coverprofile=coverage.out -covermode=atomic -count=1
	@$(GO) tool cover -func=coverage.out | tail -1
	@$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "report written to coverage.html"
	@command -v open >/dev/null && open coverage.html || true

## test-integration: run the Postgres-backed tests against the Docker database
test-integration: docker-up-infra
	@echo "waiting for postgres to accept connections..."
	@until $(COMPOSE) exec -T postgres pg_isready -U orders -d orders >/dev/null 2>&1; do sleep 1; done
	POSTGRES_TEST_DSN="postgres://orders:orders@localhost:5432/orders_test?sslmode=disable" \
		$(GO) test ./internal/order/repository/ -v -count=1 -run 'Postgres|Contract'

# -----------------------------------------------------------------------------
# Code quality
# -----------------------------------------------------------------------------

## fmt: format all Go code
fmt:
	$(GO) fmt ./...
	@command -v gofmt >/dev/null && gofmt -l -w . >/dev/null || true

## vet: run Go's built-in static analysis
vet:
	$(GO) vet ./...

## lint: run golangci-lint if it is installed
lint:
	@if command -v golangci-lint >/dev/null; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint is not installed; falling back to go vet"; \
		$(GO) vet ./...; \
	fi

## check: fmt + vet + test — run this before you commit
check: fmt vet test
	@echo ""
	@echo "all checks passed"

## ci: what a CI pipeline should run (stricter: race detector, no cache)
ci: vet
	$(GO) test -race ./... -count=1
	@cd ai && $(PYTHON) -m unittest discover -s tests -t .

# -----------------------------------------------------------------------------
# Running
# -----------------------------------------------------------------------------

## run: run the order service locally against the Docker Postgres and Redis
run:
	APP_ENV=dev \
	POSTGRES_DSN="postgres://orders:orders@localhost:5432/orders?sslmode=disable" \
	REDIS_ADDR="localhost:6379" \
	INVENTORY_URL="http://localhost:8081" \
	PAYMENT_URL="http://localhost:8082" \
	LEGACY_URL="http://localhost:8083" \
	$(GO) run ./cmd/orderd

## run-nodocker: run the order service with NO infrastructure at all
run-nodocker:
	@echo "Starting with the in-memory repository and in-process cache."
	@echo "WHAT YOU LOSE: durability (data is gone on restart), real row locks,"
	@echo "and cross-replica cache coherence. Everything else behaves identically,"
	@echo "because every dependency is behind an interface."
	@echo ""
	APP_ENV=dev \
	POSTGRES_ENABLED=false \
	REDIS_ENABLED=false \
	SAGA_ENABLED=true \
	INVENTORY_URL="" \
	PAYMENT_URL="" \
	LEGACY_URL="" \
	STRANGLER_ENABLED=false \
	LOG_LEVEL=debug \
	$(GO) run ./cmd/orderd

## run-assistant: run the Python chatbot sidecar locally
run-assistant:
	cd ai && ORDER_SERVICE_URL=http://localhost:8080 $(PYTHON) server.py

# -----------------------------------------------------------------------------
# Docker
# -----------------------------------------------------------------------------

## docker-up: build and start the entire system
docker-up:
	$(COMPOSE) up -d --build
	@echo ""
	@echo "waiting for the order service to become ready..."
	@for i in $$(seq 1 60); do \
		if curl -fsS http://localhost:8080/readyz >/dev/null 2>&1; then \
			echo "ready after $${i}s"; break; \
		fi; \
		sleep 1; \
	done
	@$(MAKE) --no-print-directory status

## docker-up-infra: start only Postgres and Redis (for local Go development)
docker-up-infra:
	$(COMPOSE) up -d postgres redis

## docker-down: stop everything, keeping the database volume
docker-down:
	$(COMPOSE) down

## docker-clean: stop everything AND DELETE the database volume
docker-clean:
	$(COMPOSE) down -v
	@echo "containers and the postgres volume have been removed"

## docker-rebuild: force a full rebuild with no layer cache
docker-rebuild:
	$(COMPOSE) build --no-cache
	$(COMPOSE) up -d

## docker-logs: tail the logs of every service
docker-logs:
	$(COMPOSE) logs -f --tail=50

## watch-logs: tail only the order service's logs
watch-logs:
	$(COMPOSE) logs -f --tail=100 order

## status: show which services are up and what each endpoint reports
status:
	@echo ""
	@echo "$(BOLD)Service health$(RESET)"
	@printf "  %-22s %s\n" "order (8080)"     "$$(curl -fsS http://localhost:8080/readyz 2>/dev/null | head -c 120 || echo 'DOWN')"
	@printf "  %-22s %s\n" "inventory (8081)" "$$(curl -fsS http://localhost:8081/readyz 2>/dev/null | head -c 80 || echo 'DOWN')"
	@printf "  %-22s %s\n" "payment (8082)"   "$$(curl -fsS http://localhost:8082/readyz 2>/dev/null | head -c 80 || echo 'DOWN')"
	@printf "  %-22s %s\n" "legacy (8083)"    "$$(curl -fsS http://localhost:8083/readyz 2>/dev/null | head -c 80 || echo 'DOWN')"
	@printf "  %-22s %s\n" "assistant (8090)" "$$(curl -fsS http://localhost:8090/readyz 2>/dev/null | head -c 120 || echo 'DOWN')"
	@echo ""
	@echo "  API docs: http://localhost:8080/docs"
	@echo ""

## stats: show order statistics, cache effectiveness and migration progress
stats:
	@echo ""
	@echo "$(BOLD)Order statistics$(RESET)"
	@curl -fsS http://localhost:8080/api/v1/orders/stats 2>/dev/null | $(PYTHON) -m json.tool || echo "  (the order service is not reachable)"
	@echo ""
	@echo "$(BOLD)Cache effectiveness$(RESET)"
	@curl -fsS http://localhost:8080/metrics 2>/dev/null | grep -E '^cache_' || echo "  (no cache metrics yet — make some requests first)"
	@echo ""
	@echo "$(BOLD)Strangler Fig migration progress$(RESET)"
	@curl -fsS http://localhost:8080/api/v1/admin/strangler 2>/dev/null | $(PYTHON) -m json.tool || true
	@echo ""

# -----------------------------------------------------------------------------
# End-to-end and demos
# -----------------------------------------------------------------------------

## e2e: run the full end-to-end test suite against the running stack
e2e:
	@./scripts/e2e.sh

## smoke: a 10-second check that the basics work
smoke:
	@./scripts/smoke.sh

## demo: a guided walkthrough of every feature, with real curl commands
demo:
	@./scripts/demo.sh

## load-test: fire concurrent requests to show the cache and locking under load
load-test:
	@./scripts/load_test.sh

# -----------------------------------------------------------------------------
# Database access
# -----------------------------------------------------------------------------

## psql: open a SQL shell on the orders database
psql:
	@echo "Try these:"
	@echo "  SELECT order_number, status, version, total_amount_minor FROM orders ORDER BY created_at DESC LIMIT 10;"
	@echo "  SELECT status, COUNT(*) FROM orders GROUP BY status;"
	@echo "  SELECT event_type, published_at IS NOT NULL AS published, COUNT(*) FROM outbox GROUP BY 1,2;"
	@echo "  SELECT source, COUNT(*) FROM orders GROUP BY source;   -- migration progress"
	@echo ""
	$(COMPOSE) exec postgres psql -U orders -d orders

## redis-cli: open a Redis shell
redis-cli:
	@echo "Try these:"
	@echo "  KEYS order:*                      -- see what is cached"
	@echo "  TTL <key>                         -- see the remaining lifetime"
	@echo "  GET order:v1:list:generation      -- the group-invalidation counter"
	@echo "  INFO memory                       -- check against the 256mb maxmemory"
	@echo ""
	$(COMPOSE) exec redis redis-cli

## migrate: apply database migrations (the services also do this at startup)
migrate: docker-up-infra
	@echo "Migrations run automatically when a service starts; this just starts one."
	@$(MAKE) --no-print-directory run

# -----------------------------------------------------------------------------
# Cleanup
# -----------------------------------------------------------------------------

## clean: remove build artefacts and coverage reports
clean:
	rm -rf bin coverage.out coverage.html
	find . -name '__pycache__' -type d -prune -exec rm -rf {} + 2>/dev/null || true
	@echo "cleaned"
