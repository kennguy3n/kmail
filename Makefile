.PHONY: build test cover lint fmt vet tidy docker-build clean help migrate bench e2e scim-test helm-lint loadtest chaos screenshots openapi storage-cost deliverability-check

# ---------------------------------------------------------------
# KMail Go control plane — developer Makefile.
#
# All targets operate on the Go module at the repo root. See
# docs/ARCHITECTURE.md §7 for the service topology.
# ---------------------------------------------------------------

GO        ?= go
GOFLAGS   ?=
PKG       ?= ./...
DOCKER    ?= docker
IMAGE     ?= kmail
TAG       ?= dev

help:
	@echo "KMail Makefile targets:"
	@echo "  build          Build all cmd/* binaries into ./bin/"
	@echo "  test           Run Go tests (go test -race $(PKG))"
	@echo "  lint           Run golangci-lint (requires golangci-lint)"
	@echo "  fmt            Run gofmt -s -w on all Go files"
	@echo "  vet            Run go vet"
	@echo "  tidy           Run go mod tidy"
	@echo "  migrate        Apply migrations/*.sql to \$$DATABASE_URL (idempotent)"
	@echo "  docker-build   Build the multi-stage Docker image"
	@echo "  e2e            Run the scripts/test-e2e.sh smoke harness"
	@echo "  screenshots    Capture demo PNGs for docs/screenshots/ (Vite + MSW)"
	@echo "  openapi        Regenerate api/openapi/kmail.openapi.json from the Go routes"
	@echo "  clean          Remove built binaries"

build:
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o ./bin/ ./cmd/...

test:
	$(GO) test $(GOFLAGS) -race $(PKG)

# WS4 Task 5 — coverage gate. Runs the suite with a coverage profile
# and fails below MIN_COVERAGE (default 30, ratchets up over time).
cover:
	./scripts/check-coverage.sh

lint:
	golangci-lint run $(PKG)

# openapi regenerates the committed OpenAPI 3.1 spec by scanning the Go
# route literals (see api/openapi/generate.mjs for how routes are
# extracted). The result is committed and consumed by the marketing
# site's Redoc page (site/src/pages/docs/api.astro); sync-content.mjs
# copies it into site/public/openapi/ at build time. Run this whenever
# you add or change an `"<METHOD> /path"` mux pattern.
NODE ?= node
openapi:
	$(NODE) api/openapi/generate.mjs

fmt:
	gofmt -s -w .

vet:
	$(GO) vet $(PKG)

tidy:
	$(GO) mod tidy

migrate:
	./scripts/migrate.sh

docker-build:
	$(DOCKER) build -t $(IMAGE):$(TAG) .

clean:
	rm -rf ./bin

# bench runs the benchmark harness against the local compose stack.
# Override BENCH_ITER to control the JMAP iteration count.
BENCH_ITER ?= 200
BENCH_SMTP_N ?= 50
BENCH_CALDAV_N ?= 50
bench:
	$(GO) run ./scripts/bench/bench-jmap.go --iterations $(BENCH_ITER)
	./scripts/bench/bench-smtp.sh $(BENCH_SMTP_N)
	./scripts/bench/bench-caldav.sh $(BENCH_CALDAV_N)

# e2e runs scripts/test-e2e.sh against the running compose stack.
# Override KMAIL_API_URL to point at a remote BFF. The default is
# `:8088` because Stalwart occupies host `:8080` (matched
# 8080:8080 publish in docker-compose.yml); see
# internal/config/config.go HTTPConfig.Addr for the full
# rationale.
KMAIL_API_URL ?= http://localhost:8088
e2e:
	KMAIL_API_URL=$(KMAIL_API_URL) ./scripts/test-e2e.sh

# scim-test runs the SCIM 2.0 conformance harness against a
# running BFF. Override KMAIL_API_URL to point at a remote
# instance. Results are documented in docs/SCIM_CONFORMANCE.md.
scim-test:
	KMAIL_API_URL=$(KMAIL_API_URL) ./scripts/test-scim.sh

# helm-lint runs `helm lint` against the deploy/helm/kmail chart.
# Requires Helm 3.x to be on PATH; in CI set HELM=/path/to/helm.
HELM ?= helm
helm-lint:
	$(HELM) lint deploy/helm/kmail

# loadtest runs the Phase 7 JMAP / SMTP load harness from
# scripts/loadtest/. Override LOADTEST_ITER / LOADTEST_TPS to
# change the workload shape.
LOADTEST_ITER ?= 1000
LOADTEST_TPS ?= 25
loadtest:
	$(GO) run ./scripts/loadtest/load-jmap.go --iterations $(LOADTEST_ITER)
	./scripts/loadtest/load-smtp.sh $(LOADTEST_TPS)

# chaos runs the Phase 7 chaos-engineering harness against the
# local compose stack. Each script kills / pauses one dependency
# in turn and verifies the BFF degrades gracefully. Run targets
# individually if you only want to exercise one failure mode.
chaos:
	./scripts/loadtest/chaos-shard.sh
	./scripts/loadtest/chaos-postgres.sh
	./scripts/loadtest/chaos-valkey.sh

# screenshots starts the React dev server with the MSW mock layer
# (VITE_MOCK_API=true) and runs scripts/capture-screenshots.mjs to
# regenerate every PNG in docs/screenshots/. The wrapper script
# manages the Vite lifecycle and verifies the expected output.
screenshots:
	./scripts/capture-screenshots-with-mock.sh

# scale-test runs the multi-tenant scale load-test harness from
# scripts/loadtest/: it seeds a synthetic tenant fleet, drives the
# weighted workload through ramp-up/steady/cool-down, and renders a
# Markdown SLO report. Override TENANTS / USERS / DURATION (steady-
# state duration) and the SCALE_* knobs as needed. Set SCALE_DRY=1
# to exercise the full pipeline (seed -> load -> report) offline
# without touching the BFF — used by the build self-check.
#
#   make scale-test TENANTS=100 USERS=10 DURATION=10m
#   make scale-test SCALE_DRY=1                 # offline dry run
.PHONY: scale-test
TENANTS        ?= 100
USERS          ?= 20
DURATION       ?= 10m
SCALE_WORKERS  ?= 64
SCALE_RAMPUP   ?= 1m
SCALE_COOLDOWN ?= 1m
SCALE_MESSAGES ?= 10000
SCALE_OUT      ?= ./loadtest-out
SCALE_DRY      ?=
SCALE_DRY_FLAG := $(if $(SCALE_DRY),--dry-run,)
scale-test:
	@mkdir -p $(SCALE_OUT)
	$(GO) run ./scripts/loadtest/seed-tenants.go \
	  --tenants $(TENANTS) --users $(USERS) --messages $(SCALE_MESSAGES) $(SCALE_DRY_FLAG)
	$(GO) run ./scripts/loadtest/scale-5k.go \
	  --tenants $(TENANTS) --workers $(SCALE_WORKERS) \
	  --rampup $(SCALE_RAMPUP) --steady $(DURATION) --cooldown $(SCALE_COOLDOWN) \
	  --json-out $(SCALE_OUT)/scale-report.json $(SCALE_DRY_FLAG)
	$(GO) run ./scripts/loadtest/report.go \
	  --in $(SCALE_OUT)/scale-report.json --out $(SCALE_OUT)/scale-report.md \
	  --fail-on-violation=$(if $(SCALE_DRY),false,true)

# scale-test-multishard drives the sharded fleet (see docs/BENCHMARKS.md
# "Multi-shard scale benchmark") and reports cross-shard routing
# latency, shard failover time, and rebalance duration. With SCALE_DRY=1
# it validates the plan + reporting path offline (the build self-check);
# a live run wants --discover against a seeded fleet. Failover/rebalance
# drills mutate fleet state, so they are OFF unless SCALE_DRILLS=1.
#
#   make scale-test-multishard SCALE_DRY=1                    # offline
#   make scale-test-multishard SHARDS=10 SCALE_DRILLS=1       # live drill
.PHONY: scale-test-multishard
SHARDS        ?= 10
SCALE_DISCOVER ?= $(if $(SCALE_DRY),,--discover)
SCALE_DRILLS  ?=
SCALE_DRILL_FLAGS := $(if $(SCALE_DRILLS),--failover --rebalance,)
scale-test-multishard:
	@mkdir -p $(SCALE_OUT)
	$(GO) run ./scripts/loadtest/scale-5k-multishard.go \
	  --tenants $(TENANTS) --shards $(SHARDS) --workers $(SCALE_WORKERS) \
	  --rampup $(SCALE_RAMPUP) --steady $(DURATION) --cooldown $(SCALE_COOLDOWN) \
	  --json-out $(SCALE_OUT)/multishard-report.json \
	  $(SCALE_DISCOVER) $(SCALE_DRILL_FLAGS) $(SCALE_DRY_FLAG)

# storage-cost models the object-storage $/user/mo against the
# ~$$0.12/user/mo projection in docs/PROPOSAL.md. Deterministic — no
# infra needed. Override the tier distribution / price via flags (see
# the script header). Writes Markdown + JSON into $(SCALE_OUT).
storage-cost:
	@mkdir -p $(SCALE_OUT)
	$(GO) run ./scripts/loadtest/storage-cost.go \
	  --md-out $(SCALE_OUT)/storage-cost.md --json-out $(SCALE_OUT)/storage-cost.json

# deliverability-check validates the local half of the email-auth
# stack: real DKIM keygen, SPF/DKIM/DMARC record generation, and a
# DKIM key-consistency proof. No IP pool / provider mailbox needed.
# Inbox-placement measurement is the documented real-infra follow-up.
DELIV_DOMAIN ?= acme.example
deliverability-check:
	@mkdir -p $(SCALE_OUT)
	$(GO) run ./scripts/loadtest/deliverability-check.go \
	  --domain $(DELIV_DOMAIN) --md-out $(SCALE_OUT)/deliverability.md
