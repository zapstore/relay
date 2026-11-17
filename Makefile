# dist/<name>-<os>-<arch>

NAME := relay
CMD := ./cmd

CGO_ENABLED ?= 1
GOFLAGS := -trimpath
TAGS ?=
LDFLAGS := -s -w

# Stamp config.Version only when HEAD is exactly a git tag.
VERSION := $(shell git describe --tags --exact-match HEAD 2>/dev/null)
ifneq ($(VERSION),)
LDFLAGS += -X github.com/zapstore/relay/pkg/config.Version=$(VERSION)
endif

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
# Zig's final link keeps the debug sections -s -w drops from the Go link.
# Darwin's ld warns that -s is obsolete, and -s -w is enough there.
ifneq ($(GOOS),darwin)
LDFLAGS += -extldflags=-s
endif
BIN := dist/$(NAME)-$(GOOS)-$(GOARCH)
LOCALBIN := $(HOME)/.local/bin

comma := ,
ifeq ($(GOOS),darwin)
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
else
# A Darwin parent exports this flag; zig rejects it when linking Linux.
CGO_LDFLAGS := $(filter-out -Wl$(comma)-no_warn_duplicate_libraries,$(CGO_LDFLAGS))
endif
export CGO_LDFLAGS

HOST_OS := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)

.DEFAULT_GOAL := help
.PHONY: help build darwin-arm64 linux-arm64 linux-amd64 clean

help: ## List targets
	@awk 'BEGIN { FS = ":.*## " } /^[a-zA-Z0-9_.-]+:.*## / { printf "  %-20s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build dist/$(NAME)-$(GOOS)-$(GOARCH); link when that matches this machine
	mkdir -p dist
	rm -rf $(BIN)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(GOFLAGS)$(if $(TAGS), -tags $(TAGS)) -ldflags '$(LDFLAGS)' \
		-o $(BIN) $(CMD)
ifeq ($(GOOS)-$(GOARCH),$(HOST_OS)-$(HOST_ARCH))
	ln -sfn $(BIN) $(NAME)
	@if [ -d "$(LOCALBIN)" ]; then \
		ln -sfn "$(CURDIR)/$(BIN)" "$(LOCALBIN)/$(NAME)" || exit 1; \
		echo "linked $(LOCALBIN)/$(NAME)"; \
	fi
endif

darwin-arm64: ## macOS arm64 → dist/ only
	$(MAKE) build GOOS=darwin GOARCH=arm64

linux-arm64: ## Linux arm64 → dist/ only
	$(MAKE) build GOOS=linux GOARCH=arm64 \
		CC="zig cc -target aarch64-linux-gnu" \
		CXX="zig c++ -target aarch64-linux-gnu"

linux-amd64: ## Linux amd64 → dist/ only
	$(MAKE) build GOOS=linux GOARCH=amd64 \
		CC="zig cc -target x86_64-linux-gnu" \
		CXX="zig c++ -target x86_64-linux-gnu"

clean: ## Remove dist/ and symlinks
	rm -rf dist
	rm -f $(NAME)
	@l="$(LOCALBIN)/$(NAME)"; \
	if [ -L "$$l" ]; then \
		case "$$(readlink "$$l")" in "$(CURDIR)/dist/"*) rm -f "$$l";; esac; \
	fi
