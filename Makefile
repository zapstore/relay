# Deploy runs: make release REF=<ref>
# Writes dist/<name>-<ref>-<arch>. Infra installs that file as releases/<id>-<ref>.

NAME := relay
REF ?=
GOARCH ?= $(shell go env GOARCH)
DIST := dist/$(NAME)-$(or $(REF),dev)-$(GOARCH)

# go-sqlite3 (fts5) and chai2010/webp both pass -lm. Apple ld warns; ignore it.
GOOS ?= $(shell go env GOOS)
ifeq ($(GOOS),darwin)
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
endif

.PHONY: release clean

release:
	mkdir -p dist
	rm -rf $(DIST)
	CGO_ENABLED=1 go build -tags fts5 -trimpath \
		-ldflags '-s -w $(if $(REF),-X github.com/zapstore/relay/pkg/config.Version=$(REF))' \
		-o $(DIST) ./cmd

clean:
	rm -rf dist
