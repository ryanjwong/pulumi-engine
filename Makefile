# pulumi-engine build entry points.
#
#   make build        compile all Go packages
#   make test         Go unit tests
#   make integration  Go integration tests (real providers, network; PULUMI_ENGINE_INTEGRATION=1)
#   make parity       the CLI parity subset of the integration tests (needs the pulumi CLI)
#   make lib          build libpulumi.{dylib,so} + header for the host platform into build/
#   make node         install, build and test the Node binding against build/libpulumi.*
#   make python       create bindings/python/.venv, install the binding, pytest against build/libpulumi.*
#   make lint         golangci-lint if installed, otherwise go vet
#   make upstream-check  diff the copied Pulumi code under internal/upstream against the pinned module
#   make upstream-update accept the pinned version in the copied files' headers (after review)
#   make bump PULUMI=vX.Y.Z  bump pkg/v3 + sdk/v3, tidy, unit tests, drift check (scripts/bump-pulumi.sh)
#   make dist-lib     dist/libpulumi-<os>-<arch>.tar.gz (+ .sha256) for the host platform
#   make dist-node    dist/npm/: the Node packages (main + host platform) as npm tarballs
#   make dist-python  dist/python/: the Python wheel and sdist
#   make release-dry-run  everything a release builds, for the host platform, into dist/ (docs/releasing.md)
#
# VERSION=X.Y.Z stamps the library (engine.LibraryVersion) and the packages;
# the release workflow derives it from the tag, local builds default to 0.0.0-dev.

GO      ?= go
PYTHON  ?= python3
BUILD   ?= build
DIST    ?= dist
VERSION ?= 0.0.0-dev
NPM_SCOPE    ?= @ryanjwong
NPM_REGISTRY ?= https://npm.pkg.github.com
GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)

VERSION_LDFLAG := -X github.com/ryanjwong/pulumi-engine/engine.LibraryVersion=$(VERSION)
ifeq ($(GOOS),darwin)
LIB_EXT := dylib
# Give the dylib an @rpath install name so consumers can locate it via rpath.
LIB_LDFLAGS := -ldflags "$(VERSION_LDFLAG) -extldflags=-Wl,-install_name,@rpath/libpulumi.dylib"
else
LIB_EXT := so
LIB_LDFLAGS := -ldflags "$(VERSION_LDFLAG)"
endif

LIB      := $(BUILD)/libpulumi.$(LIB_EXT)
HEADER   := $(BUILD)/libpulumi.h

.PHONY: build test integration parity lib abitest node node-install node-build python python-install lint clean upstream-check upstream-update bump \
	dist-lib dist-node dist-python release-dry-run

build:
	$(GO) build ./...

test:
	$(GO) test ./... -count=1

integration:
	PULUMI_ENGINE_INTEGRATION=1 $(GO) test ./engine/... -run 'Integration' -count=1 -v -timeout 30m

# CLI parity subset of the integration tests (needs `pulumi` on PATH or
# PULUMI_ENGINE_CLI=/path/to/pulumi). See docs/cli-parity.md.
parity:
	PULUMI_ENGINE_INTEGRATION=1 $(GO) test ./engine/... -run 'IntegrationParity' -count=1 -v -timeout 30m

lib: $(LIB)

$(LIB): $(shell find . -name '*.go' -not -path './bindings/*') go.mod go.sum
	mkdir -p $(BUILD)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath $(LIB_LDFLAGS) -o $(LIB) ./capi
	@echo "built $(LIB) and $(HEADER)"

# Exercises the built shared library through its C header (cgo test that
# links libpulumi). Needs `make lib` first.
abitest: lib
	$(GO) test -tags abitest ./capi/abitest -count=1 -v

node-install:
	cd bindings/nodejs && pnpm install --frozen-lockfile

node-build: lib node-install
	mkdir -p bindings/nodejs/lib/native
	# Remove before copying: overwriting a dylib in place on macOS invalidates
	# the kernel's cached code signature and every process that then loads
	# it is killed (SIGKILL) on first call.
	rm -f bindings/nodejs/lib/native/libpulumi-$(GOOS)-$(GOARCH).$(LIB_EXT)
	cp $(LIB) bindings/nodejs/lib/native/libpulumi-$(GOOS)-$(GOARCH).$(LIB_EXT)
	cd bindings/nodejs && pnpm build

node: node-build
	cd bindings/nodejs && pnpm test

PY_VENV := bindings/python/.venv

# The Python binding's virtualenv with the binding (editable) and its test
# dependencies (pytest, the Pulumi Python SDK and pulumi-random for inline
# programs).
python-install:
	@test -x $(PY_VENV)/bin/python || $(PYTHON) -m venv $(PY_VENV)
	$(PY_VENV)/bin/python -m pip install -q -e 'bindings/python[dev]'

# Runs the binding's tests against build/libpulumi.*. Tests need network on
# first run for the YAML host and the random/command providers.
python: lib python-install
	PULUMI_ENGINE_LIB=$(CURDIR)/$(LIB) $(PY_VENV)/bin/python -m pytest bindings/python/tests -x -q

# Drift check of the fork boundary (internal/upstream): fails with a unified
# diff when an upstream file a copy was taken from changed at the pinned version.
upstream-check:
	$(GO) run ./internal/upstream/cmd/upstreamcheck

upstream-update:
	$(GO) run ./internal/upstream/cmd/upstreamcheck -update

# Bump the Pulumi pin: make bump PULUMI=v3.240.0
bump:
	@test -n "$(PULUMI)" || { echo "usage: make bump PULUMI=vX.Y.Z"; exit 2; }
	./scripts/bump-pulumi.sh $(PULUMI)

# Release artifacts for the host platform (docs/releasing.md). The release
# workflow runs the same targets on one native runner per platform.
dist-lib: lib
	./scripts/dist-lib.sh $(BUILD) $(DIST) $(GOOS)-$(GOARCH)

dist-node: node-build dist-lib
	node scripts/package-node.mjs --version $(VERSION) --scope $(NPM_SCOPE) --registry $(NPM_REGISTRY) \
		--libs $(DIST) --out $(DIST)/npm --pack

dist-python: python-install
	rm -rf $(DIST)/python && mkdir -p $(DIST)/python
	cd bindings/python && .venv/bin/python -m pip wheel --no-deps -q -w ../../$(DIST)/python . \
		&& .venv/bin/python -m pip install -q build && .venv/bin/python -m build --sdist -o ../../$(DIST)/python .

release-dry-run: dist-lib abitest dist-node dist-python
	@echo; echo "release artifacts in $(DIST)/ (VERSION=$(VERSION)):"; ls -1 $(DIST) $(DIST)/npm $(DIST)/python | grep -v '^$$'

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; running go vet"; $(GO) vet ./...; fi

clean:
	rm -rf $(BUILD) $(DIST) bindings/nodejs/dist bindings/nodejs/lib/native
