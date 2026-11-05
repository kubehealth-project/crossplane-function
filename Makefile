.PHONY: test test-unit test-render vet race image package

GO ?= go
CROSSPLANE ?= crossplane
DOCKER ?= docker
PYTHON ?= python3

BUILD_DIR := $(CURDIR)/build
FUNCTION_IMAGE ?= crossplane-function:local

test: test-unit test-render

test-unit:
	$(GO) test ./...

vet:
	$(GO) vet ./...

race:
	$(GO) test -race ./...

test-render:
	@mkdir -p "$(BUILD_DIR)"
	@set -euo pipefail; \
		$(GO) run . --insecure --address=:9444 >"$(BUILD_DIR)/function.log" 2>&1 & \
		pid=$$!; \
		trap 'kill $$pid 2>/dev/null || true; wait $$pid 2>/dev/null || true' EXIT; \
		for attempt in $$(seq 1 60); do nc -z localhost 9444 >/dev/null 2>&1 && break; sleep 1; done; \
		nc -z localhost 9444 >/dev/null 2>&1 || { printf 'Function did not become ready\n' >&2; exit 1; }; \
		$(CROSSPLANE) render example/xr.yaml example/composition.yaml example/functions.yaml --include-full-xr >"$(BUILD_DIR)/render-pending.yaml"; \
		$(PYTHON) example/assert_render.py "$(BUILD_DIR)/render-pending.yaml" --reconciliation InProgress --availability Unknown; \
		$(CROSSPLANE) render example/observed/healthy.yaml example/composition.yaml example/functions.yaml --include-full-xr --observed-resources example/observed/healthy.yaml >"$(BUILD_DIR)/render-healthy.yaml"; \
		$(PYTHON) example/assert_render.py "$(BUILD_DIR)/render-healthy.yaml" --reconciliation Reconciled --availability Available; \
		$(CROSSPLANE) render example/observed/failed-initial.yaml example/composition.yaml example/functions.yaml --include-full-xr --observed-resources example/observed/failed-initial.yaml >"$(BUILD_DIR)/render-failed-initial.yaml"; \
		$(PYTHON) example/assert_render.py "$(BUILD_DIR)/render-failed-initial.yaml" --reconciliation Failed --availability Unavailable; \
		$(CROSSPLANE) render example/observed/failed-update.yaml example/composition.yaml example/functions.yaml --include-full-xr --observed-resources example/observed/failed-update.yaml >"$(BUILD_DIR)/render-failed-update.yaml"; \
		$(PYTHON) example/assert_render.py "$(BUILD_DIR)/render-failed-update.yaml" --reconciliation Failed --availability Available

image:
	$(DOCKER) build -t "$(FUNCTION_IMAGE)" .

package: image
	@mkdir -p "$(BUILD_DIR)"
	$(CROSSPLANE) xpkg build \
		--package-root package \
		--embed-runtime-image "$(FUNCTION_IMAGE)" \
		--package-file "$(BUILD_DIR)/crossplane-function.xpkg"
