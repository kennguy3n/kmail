.PHONY: build test lint fmt vet tidy docker-build clean help migrate bench e2e scim-test helm-lint loadtest chaos screenshots

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
	@echo "  clean          Remove built binaries"

build:
	@mkdir -p bin
	$(GO) build $(GOFLAGS) -o ./bin/ ./cmd/...

test:
	$(GO) test $(GOFLAGS) -race $(PKG)

lint:
	golangci-lint run $(PKG)

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
# Override KMAIL_API_URL to point at a remote BFF.
KMAIL_API_URL ?= http://localhost:8080
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
