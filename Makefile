GO ?= go
BIN := bin
DIST ?= dist

.PHONY: all build test fmt vet tidy run clean release

all: fmt vet test build

# remote --version and the agent_version a node reports to routers.
LDFLAGS = -X remote.network/node/internal/cli.Version=$(VERSION)

build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/remote ./cmd/remote

run:
	$(GO) run ./cmd/remote node start --router ws://localhost:8080/node/v1/connect

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

# Release artifacts: cross-compiled binaries + checksums. Static and
# cgo-free. Each binary is written twice: remote-<version>-<os>-<arch>
# (what a release keeps) and remote-<os>-<arch> (the stable name
# scripts/install.sh downloads from releases/latest/download).
# Windows nodes: the agent runs (Ollama/vLLM backends); managed
# llama-server mode stays darwin/linux until llama.cpp windows assets
# are mapped.
release:
	mkdir -p $(DIST)
	for plat in $(PLATFORMS); do \
		os=$${plat%/*}; arch=$${plat#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/remote-$(VERSION)-$$os-$$arch$$ext ./cmd/remote && \
		cp $(DIST)/remote-$(VERSION)-$$os-$$arch$$ext $(DIST)/remote-$$os-$$arch$$ext; \
	done
	cd $(DIST) && shasum -a 256 remote-* > checksums.txt
	@echo "$(DIST)/:" && ls $(DIST)

clean:
	rm -rf $(BIN) $(DIST)
