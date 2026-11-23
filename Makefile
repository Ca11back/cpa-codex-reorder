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

.PHONY: build test vet smoke release-lib release-package clean

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

# Store release: dist/lib/linux_<arch>/codex-reorder.so -> dist/release/*.zip + checksums.txt.
# CI (.github/workflows/release.yml) runs both steps for amd64 and arm64.
VERSION ?= $(shell sed -n 's/^[[:space:]]*pluginVersion[[:space:]]*=[[:space:]]*"\(.*\)"/\1/p' plugin.go)
RELEASE_PLATFORMS ?= darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64
comma := ,
empty :=
space := $(empty) $(empty)
release_extension_darwin := dylib
release_extension_linux := so
release_extension_windows := dll
release_lib = dist/lib/$(subst /,_,$(1))/$(NAME).$(release_extension_$(firstword $(subst /, ,$(1))))

# Release library for the host architecture, built in manylinux2014 (glibc 2.17). Needs docker.
release-lib:
	scripts/build-manylinux.sh

# Package the libraries for the CPA plugin store and verify them with CPA's own installer.
release-package:
	$(GO) run ./tools/release package -version $(VERSION) $(foreach p,$(RELEASE_PLATFORMS),-lib $(p)=$(call release_lib,$(p)))
	$(GO) run ./tools/release verify -version $(VERSION) -require $(subst $(space),$(comma),$(RELEASE_PLATFORMS))

clean:
	rm -rf dist $(SMOKE_DIR)
