# Deploy runs: make release REF=<tag>
# Writes dist/out from this tree. Does not check out git.

REF ?=

# go-sqlite3 (fts5) and chai2010/webp both pass -lm. Apple ld warns; ignore it.
ifeq ($(shell uname -s),Darwin)
CGO_LDFLAGS += -Wl,-no_warn_duplicate_libraries
export CGO_LDFLAGS
endif

.PHONY: release clean

release:
	mkdir -p dist
	rm -rf dist/out
	CGO_ENABLED=1 go build -tags fts5 -trimpath \
		-ldflags '-s -w $(if $(REF),-X github.com/zapstore/relay/pkg/config.Version=$(REF))' \
		-o dist/out ./cmd

clean:
	rm -rf dist
