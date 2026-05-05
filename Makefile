# QUICochet — top-level Makefile (builds tunnel + panel)

VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD   := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD)
GOFLAGS := -trimpath
PREFIX  ?= /usr/local

.PHONY: all build tunnel panel run-panel clean install vet fmt test release

all: build

build: tunnel panel

tunnel:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o quiccochet ./cmd/quiccochet

panel:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o qcc-panel  ./cmd/qcc-panel

run-panel: panel
	./qcc-panel panel --config /etc/quiccochet/panel.json

vet:
	go vet ./...

fmt:
	gofmt -s -w .

test:
	go test ./...

clean:
	rm -f quiccochet qcc-panel checksums.txt
	rm -rf dist/

install: build
	install -m 0755 quiccochet $(PREFIX)/bin/quiccochet
	install -m 0755 qcc-panel  $(PREFIX)/bin/qcc-panel

uninstall:
	rm -f $(PREFIX)/bin/quiccochet $(PREFIX)/bin/qcc-panel

# Cross-compile a release archive containing BOTH binaries
release: clean
	mkdir -p dist
	@for arch in amd64 arm64; do \
	  echo "→ building linux/$$arch"; \
	  mkdir -p dist/linux-$$arch; \
	  GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/linux-$$arch/quiccochet ./cmd/quiccochet; \
	  GOOS=linux GOARCH=$$arch go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o dist/linux-$$arch/qcc-panel  ./cmd/qcc-panel; \
	  cp scripts/install.sh README.md PANEL.md dist/linux-$$arch/; \
	  chmod +x dist/linux-$$arch/install.sh; \
	  tar -czf dist/quiccochet-linux-$$arch.tar.gz -C dist/linux-$$arch .; \
	done
	@echo
	@echo "Release archives:"
	@ls -lh dist/*.tar.gz
