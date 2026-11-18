SHELL := /usr/bin/env
.SHELLFLAGS = bash -e -o pipefail -c
.DEFAULT_GOAL := help
.NOTPARALLEL:
.SILENT: # use set -v to print commands executed
.ONESHELL:

ifndef VERBOSE
MAKEFLAGS += --no-print-directory
endif

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

# The Go version is pinned once, on the go line of go.mod.
GO_VERSION := $(shell awk '$$1 == "go" { print $$2; exit }' go.mod)
export GOTOOLCHAIN := go$(GO_VERSION)+auto
export GOPROXY ?= https://proxy.golang.org,direct
GO := $(shell command -v go)

# Every target but help needs go.
ifneq ($(filter-out help,$(or $(MAKECMDGOALS),help)),)
ifeq ($(GO),)
$(error go is not on PATH. Install Go from https://go.dev/dl (any Go 1.21 or newer downloads the pinned go$(GO_VERSION) toolchain), then run `make setup`)
endif
endif

MAIN ?= ./cmd/factory
EXE ?= ./build/factory
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
GOLINT_ARGS ?= --config .golangci.yml

# Rejected in a pull request's title, description, and commit messages.
ATTRIBUTION_PATTERN := generated (with|by) \[?claude|co-authored-by:.*(claude|anthropic)|claude\.ai/code/session_

##@ Development Environment

.PHONY: setup
setup: ## Download modules and build the dev tools pinned in go.mod
	$(GO) mod download
	$(GO) build tool
	echo "Ready. Run \`make help\` to see the targets."

.PHONY: watch
watch: ## Rebuild and restart ./build/factory on every change
	$(GO) tool air \
		--build.cmd "$(MAKE) build" \
		--build.entrypoint "$(EXE)" \
		--build.exclude_dir ".github,build,docs,tmp"

##@ Build

.PHONY: build
build: build/go ## Build everything

.PHONY: build/go
build/go: ## Build ./build/factory with the version stamped in
	mkdir -p ./build
	$(GO) build -ldflags "$(LDFLAGS)" -o $(EXE) $(MAIN)

.PHONY: clean
clean: ## Remove build output
	rm -rf ./build ./tmp

##@ Test

.PHONY: test
test: ## Run the tests
	$(GO) test ./...

.PHONY: test/race
test/race: ## Run the tests with the race detector
	$(GO) test -race ./...

##@ Code Quality

.PHONY: check
check: check/format check/tidy check/lint test ## Check formatting, tidiness, lint, and tests without changing files

.PHONY: check/format
check/format: ## Fail if any Go file needs formatting
	$(GO) tool golangci-lint fmt --diff $(GOLINT_ARGS) || { echo "Files need formatting (run \`make format\`)."; exit 1; }

.PHONY: check/lint
check/lint: ## Run golangci-lint without fixing
	$(GO) tool golangci-lint run $(GOLINT_ARGS) ./... || { echo "Lint failed (run \`make fix\` for what can be fixed automatically)."; exit 1; }

.PHONY: check/tidy
check/tidy: ## Fail if go mod tidy would change go.mod or go.sum
	$(GO) mod tidy -diff || { echo "go.mod and go.sum are not tidy (run \`make tidy\`)."; exit 1; }

.PHONY: check/vuln
check/vuln: ## Fail on reachable known vulnerabilities (needs network, so not part of make check)
	$(GO) tool govulncheck ./...

# Not part of make check: it needs a pull request number and the network.
.PHONY: check/attribution
check/attribution: ## Fail if a pull request carries AI attribution (PR=number)
	if [[ -z "$(PR)" ]]; then
		echo "Usage: make check/attribution PR=<pull request number>"
		exit 2
	fi
	if ! command -v gh >/dev/null 2>&1; then
		echo "gh is not installed. Install the GitHub CLI (https://cli.github.com), then run \`gh auth login\`."
		exit 1
	fi
	# One line per line of the title, the description, and each commit message,
	# labeled with where it came from, so a match says what to edit.
	if ! lines="$$(gh pr view "$(PR)" --json title,body,commits --jq '"title: " + .title, (.body | split("\n")[] | "description: " + .), (.commits[] | "commit \(.oid[0:8]): " + (.messageHeadline, (.messageBody | split("\n")[])))')"; then
		echo "Could not read pull request #$(PR). Check the number, and run \`gh auth login\` (in CI, set GH_TOKEN)."
		exit 1
	fi
	if hits="$$(printf '%s\n' "$$lines" | tr -d '\r' | grep -iE -- '$(ATTRIBUTION_PATTERN)')"; then
		echo "Pull request #$(PR) carries AI attribution:"
		printf '%s\n' "$$hits" | sed 's/^/  /'
		echo "Remove it: edit the pull request title or description, or amend the commit and push. Then re-run this check."
		exit 1
	fi
	echo "Pull request #$(PR) carries no AI attribution."

.PHONY: format
format: ## Format Go code (gci, gofumpt, goimports)
	$(GO) tool golangci-lint fmt $(GOLINT_ARGS)

.PHONY: fix
fix: ## Apply lint autofixes, then format
	$(GO) tool golangci-lint run --fix $(GOLINT_ARGS) ./...
	$(MAKE) format

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

##@ Helpers

.PHONY: help
help: ## Show this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_\/-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
