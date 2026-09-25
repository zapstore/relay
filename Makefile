# Deploy runs: make release REF=<ref>
# Writes dist/<name>-<ref>-<arch>. Infra installs that file as releases/<id>-<ref>.

NAME := relay
REF ?=
GOARCH ?= $(shell go env GOARCH)
DIST := dist/$(NAME)-$(or $(REF),dev)-$(GOARCH)

.PHONY: release clean

release:
	mkdir -p dist
	rm -rf $(DIST)
	CGO_ENABLED=1 go build -tags fts5 -trimpath \
		-ldflags '-s -w $(if $(REF),-X github.com/zapstore/relay/pkg/config.Version=$(REF))' \
		-o $(DIST) ./cmd

clean:
	rm -rf dist
