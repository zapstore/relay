# dist/<name>-<os>-<arch>

NAME := relay
CMD := ./cmd

CGO_ENABLED ?= 1
GOFLAGS := -trimpath
LDFLAGS := -s -w

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
BIN := dist/$(NAME)-$(GOOS)-$(GOARCH)

ifeq ($(GOOS),darwin)
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
endif

.PHONY: bin darwin-arm64 linux-arm64 linux-amd64 clean

bin:
	mkdir -p dist
	rm -rf $(BIN)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) \
		go build $(GOFLAGS) -tags fts5 -ldflags '$(LDFLAGS)' \
		-o $(BIN) $(CMD)

darwin-arm64:
	$(MAKE) bin GOOS=darwin GOARCH=arm64

linux-arm64:
	$(MAKE) bin GOOS=linux GOARCH=arm64 \
		CC="zig cc -target aarch64-linux-gnu" \
		CXX="zig c++ -target aarch64-linux-gnu"

linux-amd64:
	$(MAKE) bin GOOS=linux GOARCH=amd64 \
		CC="zig cc -target x86_64-linux-gnu" \
		CXX="zig c++ -target x86_64-linux-gnu"

clean:
	rm -rf dist
