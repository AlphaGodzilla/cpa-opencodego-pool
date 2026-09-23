# Build the CLIProxyAPI plugin shared object.
#
# Mirrors examples/plugin/Makefile from CLIProxyAPI: c-shared build mode, with the
# generated C header removed because the host resolves symbols by name.

PLUGIN_ID := opencodego-pool
BIN_DIR   := bin

GOOS   := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)

ifeq ($(GOOS),darwin)
PLUGIN_EXT := dylib
else ifeq ($(GOOS),windows)
PLUGIN_EXT := dll
else
PLUGIN_EXT := so
endif

ARTIFACT := $(BIN_DIR)/$(PLUGIN_ID).$(PLUGIN_EXT)

# Where `make install` copies the artifact. Override for the flat plugins/ layout:
#   make install INSTALL_DIR=/path/to/cpa/plugins
INSTALL_DIR ?= plugins/$(GOOS)/$(GOARCH)

.PHONY: build test race vet fmt check abicheck install clean

build:
	@mkdir -p $(BIN_DIR)
	go build -buildmode=c-shared -o $(ARTIFACT) .
	@rm -f $(BIN_DIR)/$(PLUGIN_ID).h
	@echo "built $(ARTIFACT)"
	@nm -g $(ARTIFACT) 2>/dev/null | grep -E 'cliproxy_plugin_init|cliproxyPlugin(Call|Free|Shutdown)' || true

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test

# Load the freshly built shared object through the real C ABI: init, register,
# a scheduler pick, a quota fetch, shutdown. This is the only check that catches
# a symbol or JSON-envelope mismatch, which would otherwise fail silently on load.
abicheck: build
	go run ./cmd/abicheck $(ARTIFACT)

install: build
	@mkdir -p $(INSTALL_DIR)
	cp $(ARTIFACT) $(INSTALL_DIR)/
	@echo "installed $(INSTALL_DIR)/$(PLUGIN_ID).$(PLUGIN_EXT)"

clean:
	rm -rf $(BIN_DIR)
