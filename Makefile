BUILD_DIR := build
GO_TAGS := -tags fts5
LDFLAGS := -s -w
TAG ?= $(shell git describe --tags --abbrev=0 2>/dev/null)

# Build from current checkout; TAG handling happens in the recipe below.

.PHONY: all clean relay

all: relay

relay:
	@echo "Building relay at tag $(TAG)"
	@mkdir -p $(BUILD_DIR)
	@set -e; \
	if [ -z "$(TAG)" ]; then \
		echo "No tags found" >&2; \
		exit 1; \
	fi; \
	if ! git rev-parse -q --verify "refs/tags/$(TAG)" >/dev/null; then \
		echo "Tag $(TAG) not found" >&2; \
		exit 1; \
	fi; \
	if [ -n "$(TAG)" ]; then \
		ORIG_REF="$$(git rev-parse --abbrev-ref HEAD)"; \
		ORIG_SHA="$$(git rev-parse HEAD)"; \
		RESTORE() { \
			if [ "$$ORIG_REF" = "HEAD" ]; then \
				git checkout -q "$$ORIG_SHA"; \
			else \
				git checkout -q "$$ORIG_REF"; \
			fi; \
		}; \
		trap 'RESTORE' EXIT; \
		git -c advice.detachedHead=false checkout -q "$(TAG)"; \
	fi; \
	CGO_ENABLED=1 \
		go build $(GO_TAGS) -ldflags "$(LDFLAGS)" \
		-o $(BUILD_DIR)/relay-$(TAG) ./cmd/; \
	echo "Build relay commit hash $$(git rev-parse HEAD), $$(git log -1 --pretty=%s)"

clean:
	rm -rf $(BUILD_DIR)
