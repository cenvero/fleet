GO ?= go
GOFLAGS ?=
GOCACHE ?= /tmp/cenvero-go-build-cache
GOMODCACHE ?= /tmp/cenvero-go-mod-cache

.PHONY: fmt test build tidy scale release-ready

fmt:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) gofmt -w $$(find . -name '*.go' -print)

test:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) test ./...

build:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) build ./cmd/fleet ./cmd/fleet-agent

tidy:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) $(GO) mod tidy

scale:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./scripts/run-scale-validation.sh

release-ready:
	GOCACHE=$(GOCACHE) GOMODCACHE=$(GOMODCACHE) ./scripts/run-release-readiness.sh
