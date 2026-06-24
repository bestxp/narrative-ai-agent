APP        := bot
BIN_DIR    := bin
MAIN_PKG   := ./cmd/$(APP)
LDFLAGS    := -s -w
WEB_DIR    := internal/messaging/wschat/web

MAIN_BRANCH := origin/master
LINT_ARGS   := --build-tags=integration

PLATFORMS  := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64 windows-arm64

.PHONY: help build build-all test test-race vet tidy clean web web-dev $(addprefix build-,$(PLATFORMS))

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

HOST_PLATFORM := $(shell uname -s | tr '[:upper:]' '[:lower:]')-$(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

build: web build-$(HOST_PLATFORM) ## Build web assets + binary for the current host

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

web: ## Build the wschat React app into web/dist (embedded via go:embed)
	cd $(WEB_DIR) && npm install --no-audit --no-fund && npm run build

web-dev: ## Run the wschat React app in Vite dev mode (proxy to :8090)
	cd $(WEB_DIR) && npm run dev

.PHONY: lint-go-install-force
lint-go-install-force:
	@go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

.PHONY: lint-go-install
lint-go-install:
	@if ! command -v golangci-lint &> /dev/null; then \
		echo "golangci-lint is not installed."; \
		$(MAKE) lint-go-install-force; \
	fi
	@echo "use $$(golangci-lint --version)"

.PHONY: lint-go
lint-go: lint-go-install
	@echo -e "$(GREEN)go$(NC)"
	@golangci-lint run \
		$(LINT_ARGS) \
		--output.code-climate.path=gl-code-quality-report.json \
		--output.text.path=stdout \
		--output.text.print-linter-name \
		--output.text.print-issued-lines \
		--output.text.colors

.PHONY: lint-go-new
lint-go-new: lint-go-install
	@echo -e "$(GREEN)go-new$(NC)"
	@golangci-lint run \
		$(LINT_ARGS) \
		--new-from-merge-base=$(MAIN_BRANCH)
