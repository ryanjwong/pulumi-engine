# pulumi-engine build entry points.
#
#   make build        compile all Go packages
#   make test         Go unit tests
#   make integration  Go integration tests (real providers, network; PULUMI_ENGINE_INTEGRATION=1)
#   make parity       the CLI parity subset of the integration tests (needs the pulumi CLI)
#   make lib          build libpulumi.{dylib,so} + header for the host platform into build/
#   make node         install, build and test the Node binding against build/libpulumi.*
#   make lint         golangci-lint if installed, otherwise go vet

GO      ?= go
BUILD   ?= build
GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)

ifeq ($(GOOS),darwin)
LIB_EXT := dylib
# Give the dylib an @rpath install name so consumers can locate it via rpath.
LIB_LDFLAGS := -ldflags "-extldflags=-Wl,-install_name,@rpath/libpulumi.dylib"
else
LIB_EXT := so
LIB_LDFLAGS :=
endif

LIB      := $(BUILD)/libpulumi.$(LIB_EXT)
HEADER   := $(BUILD)/libpulumi.h

.PHONY: build test integration parity lib abitest node node-install node-build lint clean

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

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; running go vet"; $(GO) vet ./...; fi

clean:
	rm -rf $(BUILD) bindings/nodejs/dist bindings/nodejs/lib/native
