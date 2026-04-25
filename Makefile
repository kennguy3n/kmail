.PHONY: build test lint fmt vet tidy docker-build clean help migrate bench e2e

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
