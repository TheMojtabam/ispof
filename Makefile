# Ispof Makefile
# ----------------------------------------------------------------------
# Common targets:
#   make build          — build for the host platform → ./ispof
#   make release        — cross-compile for linux/amd64, linux/arm64, linux/armv7
#   make run            — go run with sensible defaults
#   make test           — go test ./...
#   make clean          — rm dist/ and the local binary
#
# Variables you can override:
#   make VERSION=v0.1.0 COMMIT=abc1234 release

VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo "v0.1.0")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/TheMojtabam/ispof/cmd/ispof
LDFLAGS := -s -w \
    -X main.Version=$(VERSION) \
    -X main.Commit=$(COMMIT) \
    -X main.BuildDate=$(DATE)

GO      ?= go
GOFLAGS ?= -trimpath

.PHONY: help build run test vet tidy release clean install fmt

help:
	@awk '/^[a-zA-Z_-]+:.*?##/ {split($$0,a,"##"); printf "  %-12s %s\n", a[1], a[2]}' $(MAKEFILE_LIST)

build: ## build the panel for the host platform
	$(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o ispof $(PKG)
	@echo "→ ./ispof ($(VERSION))"

run: build ## build + run with sane dev defaults
	./ispof --listen 127.0.0.1:2095 --log-level debug

test: ## run unit tests
	$(GO) test ./...

vet: ## go vet
	$(GO) vet ./...

tidy: ## go mod tidy
	$(GO) mod tidy

fmt: ## go fmt
	$(GO) fmt ./...

# release builds for the three Linux architectures the CI publishes.
# Outputs go to dist/ with matching .sha256 sidecars.
release: clean
	@mkdir -p dist
	@for target in linux/amd64 linux/arm64 linux/arm/7; do \
	    os=$$(echo $$target | cut -d/ -f1); \
	    arch=$$(echo $$target | cut -d/ -f2); \
	    arm=$$(echo $$target | cut -d/ -f3); \
	    arch_label=$$arch; \
	    if [ -n "$$arm" ]; then arch_label=armv$$arm; fi; \
	    echo "→ building $$os/$$arch_label"; \
	    GOOS=$$os GOARCH=$$arch GOARM=$$arm CGO_ENABLED=0 \
	        $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o ispof $(PKG); \
	    ARCHIVE="ispof-$(VERSION)-$$os-$$arch_label.tar.gz"; \
	    tar -czf "dist/$$ARCHIVE" ispof; \
	    ( cd dist && sha256sum "$$ARCHIVE" > "$$ARCHIVE.sha256" ); \
	    rm -f ispof; \
	done
	@echo "→ dist/"
	@ls -la dist/

install: build ## install to /usr/local/bin (requires sudo)
	install -m 0755 ispof /usr/local/bin/ispof
	@echo "→ /usr/local/bin/ispof"

clean: ## remove build artifacts
	rm -rf dist ispof

# tag VERSION=v0.1.0 — creates a tag and pushes it. CI picks it up
# and publishes a stable GitHub release with binaries for all arches.
# Use this instead of clicking around in the GitHub UI.
tag: ## create + push a release tag (usage: make tag VERSION=v0.1.0)
	@if [ -z "$(VERSION)" ] || [ "$(VERSION)" = "$(shell git describe --tags --dirty --always 2>/dev/null || echo)" ]; then \
	    echo "usage: make tag VERSION=v0.1.0"; \
	    echo "(VERSION must be set explicitly; do not rely on git describe)"; \
	    exit 1; \
	fi
	@echo "→ tagging $(VERSION)"
	git tag -a $(VERSION) -m "Release $(VERSION)"
	git push origin $(VERSION)
	@echo "→ tag pushed — CI will build and release in ~3-5 minutes:"
	@echo "  https://github.com/TheMojtabam/ispof/actions"
	@echo "  https://github.com/TheMojtabam/ispof/releases"

# verify-build — replicates the CI build locally so you can catch
# compile errors before pushing.
verify-build: ## run the same build CI runs (all 3 architectures)
	@$(MAKE) release VERSION=$(VERSION)
	@echo "✓ local build matches CI"
