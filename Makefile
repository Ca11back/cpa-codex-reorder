GO        ?= go
NAME      := codex-reorder
OUT       ?= dist/$(NAME).so
# Local CPA source tree, only needed for `make smoke` when no CPA binary is given.
CPA_SRC   ?= ../CLIProxyAPI
SMOKE_DIR := test/smoke/run
CPA_BIN   ?= $(SMOKE_DIR)/cpa

# Pin the baseline instruction set so the library loads on older CPUs and VMs
# (see CLIProxyAPI examples/plugin/README.md).
ARCH := $(shell $(GO) env GOARCH)
ifeq ($(ARCH),amd64)
PORTABLE := GOAMD64=v1 CGO_CFLAGS="-march=x86-64"
endif
ifeq ($(ARCH),arm64)
PORTABLE := GOARM64=v8.0 CGO_CFLAGS="-march=armv8-a"
endif

.PHONY: build test vet smoke clean

build:
	@mkdir -p $(dir $(OUT))
	CGO_ENABLED=1 $(PORTABLE) $(GO) build -buildmode=c-shared -trimpath -ldflags="-s -w" -o $(OUT) .
	@rm -f $(basename $(OUT)).h
	@echo "built $(OUT)"

test:
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...

# End-to-end check: runs the plugin inside a real CPA binary against a mock Codex upstream.
smoke:
	@mkdir -p $(SMOKE_DIR)/plugins
	@if [ ! -x "$(CPA_BIN)" ]; then \
		echo "building CPA from $(CPA_SRC)"; \
		(cd $(CPA_SRC) && $(GO) build -o $(abspath $(CPA_BIN)) ./cmd/server); \
	fi
	$(MAKE) build OUT=$(SMOKE_DIR)/plugins/$(NAME).so
	$(GO) run ./test/smoke -cpa $(CPA_BIN) -plugin $(SMOKE_DIR)/plugins/$(NAME).so -run $(SMOKE_DIR)

clean:
	rm -rf dist $(SMOKE_DIR)
