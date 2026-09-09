# pulumi-engine build entry points.
#
#   make build        compile all Go packages
#   make test         Go unit tests
#   make integration  Go integration tests (real providers, network; PULUMI_ENGINE_INTEGRATION=1)
#   make lib          build libpulumi.{dylib,so} + header for the host platform into build/
#   make node         install, build and test the Node binding against build/libpulumi.*
#   make lint         golangci-lint if installed, otherwise go vet

GO      ?= go
BUILD   ?= build
GOOS    := $(shell $(GO) env GOOS)
GOARCH  := $(shell $(GO) env GOARCH)

ifeq ($(GOOS),darwin)
LIB_EXT := dylib
else
LIB_EXT := so
endif

LIB      := $(BUILD)/libpulumi.$(LIB_EXT)
HEADER   := $(BUILD)/libpulumi.h

.PHONY: build test integration lib node node-install lint clean

build:
	$(GO) build ./...

test:
	$(GO) test ./... -count=1

integration:
	PULUMI_ENGINE_INTEGRATION=1 $(GO) test ./engine/... -run 'Integration' -count=1 -v -timeout 30m

lib: $(LIB)

$(LIB): $(shell find . -name '*.go' -not -path './bindings/*') go.mod go.sum
	mkdir -p $(BUILD)
	CGO_ENABLED=1 $(GO) build -buildmode=c-shared -trimpath -o $(LIB) ./capi
	@echo "built $(LIB) and $(HEADER)"

node-install:
	cd bindings/nodejs && pnpm install --frozen-lockfile

node: lib node-install
	mkdir -p bindings/nodejs/lib/native
	cp $(LIB) bindings/nodejs/lib/native/libpulumi-$(GOOS)-$(GOARCH).$(LIB_EXT)
	cd bindings/nodejs && pnpm build && pnpm test

lint:
	@if command -v golangci-lint >/dev/null 2>&1; then golangci-lint run ./...; else echo "golangci-lint not installed; running go vet"; $(GO) vet ./...; fi

clean:
	rm -rf $(BUILD) bindings/nodejs/dist bindings/nodejs/lib/native
