# KMail — multi-stage Go build.
#
# Builds every cmd/* binary and copies them into a minimal runtime
# image. The concrete service to run is selected by the container
# command (e.g., `kmail-api`, `kmail-tenant`).

# ---------------------------------------------------------------
# Stage 1: build
# ---------------------------------------------------------------
FROM golang:1.25-alpine AS build

WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod ./
RUN go mod download

COPY . .

ENV CGO_ENABLED=0 GOOS=linux GOFLAGS=-trimpath
RUN mkdir -p /out && go build -ldflags="-s -w" -o /out/ ./cmd/...

# ---------------------------------------------------------------
# Stage 2: runtime
# ---------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 kmail

COPY --from=build /out/ /usr/local/bin/

USER kmail

# Default command; override with e.g. `kmail-tenant`, `kmail-dns`.
ENTRYPOINT ["/usr/local/bin/kmail-api"]
