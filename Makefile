APP        := bot
BIN_DIR    := bin
MAIN_PKG   := ./cmd/$(APP)
LDFLAGS    := -s -w

PLATFORMS  := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64

.PHONY: help build build-all test test-race vet tidy clean $(addprefix build-,$(PLATFORMS))

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

HOST_PLATFORM := $(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

build: build-$(HOST_PLATFORM) ## Build for the current host

build-all: $(addprefix build-,$(PLATFORMS)) ## Build the full platform matrix

$(addprefix build-,$(PLATFORMS)): ## Build a single target, e.g. build-darwin-arm64
	@mkdir -p $(BIN_DIR)
	@os=$$(echo $@ | sed 's/^build-//' | cut -d- -f1); \
	arch=$$(echo $@ | sed 's/^build-//' | cut -d- -f2); \
	ext=$$( [ "$$os" = "windows" ] && echo ".exe" || echo "" ); \
	echo ">> building $$os/$$arch"; \
	GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
		go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(APP)-$$os-$$arch$$ext $(MAIN_PKG)

test: ## Run the full test suite
	go test -count=1 -timeout 60s ./...

test-race: ## Run tests with the race detector
	go test -count=1 -race -timeout 120s ./...

vet: ## Run go vet
	go vet ./...

tidy: ## Sync go.mod / go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)
