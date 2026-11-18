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

# The development Postgres from compose.yaml. CI runs the same user, password and
# database as a service container, so this URL reaches both.
POSTGRES_PORT ?= 5432
export POSTGRES_PORT
TEST_DATABASE_URL ?= postgres://factory:factory@127.0.0.1:$(POSTGRES_PORT)/factory?sslmode=disable

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

.PHONY: serve/postgres
serve/postgres: ## Start the development Postgres from compose.yaml in the background (stop: make clean/postgres)
	if ! docker info >/dev/null 2>&1; then
		echo "Docker is not running. Start Docker Desktop (or the docker daemon), then run \`make serve/postgres\` again."
		exit 1
	fi
	docker compose up --detach --wait postgres
	echo "Postgres is up on 127.0.0.1:$(POSTGRES_PORT). Run \`make test/db\`, or export FACTORY_DATABASE_URL='$(TEST_DATABASE_URL)'."

.PHONY: clean/postgres
clean/postgres: ## Stop the development Postgres and keep its data
	docker compose down

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

# -count=1 because the database is not an input go's test cache can see.
.PHONY: test/db
test/db: ## Run every test, the database tests included, against Postgres (start it with make serve/postgres)
	FACTORY_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -race -count=1 ./...

.PHONY: test/race
test/race: ## Run the tests with the race detector
	$(GO) test -race ./...

##@ Code Quality

.PHONY: check
check: check/format check/tidy check/lint check/sqlc check/migrations test ## Check formatting, tidiness, lint, and tests without changing files

.PHONY: check/format
check/format: ## Fail if any Go file needs formatting
	$(GO) tool golangci-lint fmt --diff $(GOLINT_ARGS) || { echo "Files need formatting (run \`make format\`)."; exit 1; }

.PHONY: check/lint
check/lint: ## Run golangci-lint without fixing
	$(GO) tool golangci-lint run $(GOLINT_ARGS) ./... || { echo "Lint failed (run \`make fix\` for what can be fixed automatically)."; exit 1; }

.PHONY: check/tidy
check/tidy: ## Fail if go mod tidy would change go.mod or go.sum
	$(GO) mod tidy -diff || { echo "go.mod and go.sum are not tidy (run \`make tidy\`)."; exit 1; }

.PHONY: check/sqlc
check/sqlc: ## Fail if the generated sqlc code in internal/db is stale
	$(GO) tool sqlc diff || { echo "Generated sqlc code is stale (run \`make generate/sqlc\` and commit the result)."; exit 1; }

.PHONY: check/migrations
check/migrations: ## Fail if a migration lacks its up or down file, or the numbering has a gap
	cd internal/db/migrations
	expected=1
	for up in $$(ls *.up.sql | sort); do
		number=$${up%%_*}
		if [[ ! "$$number" =~ ^[0-9]{3}$$ ]]; then
			echo "internal/db/migrations/$$up: name it NNN_name.up.sql with a three-digit number."
			exit 1
		fi
		if (( 10#$$number != expected )); then
			echo "internal/db/migrations/$$up: expected number $$(printf %03d $$expected). Renumber it so there is no gap or repeat."
			exit 1
		fi
		if [[ ! -f "$${up%.up.sql}.down.sql" ]]; then
			echo "internal/db/migrations/$$up has no $${up%.up.sql}.down.sql. Add one that undoes it."
			exit 1
		fi
		expected=$$((expected + 1))
	done
	for down in *.down.sql; do
		if [[ ! -f "$${down%.down.sql}.up.sql" ]]; then
			echo "internal/db/migrations/$$down has no $${down%.down.sql}.up.sql. Add it or delete the down file."
			exit 1
		fi
	done

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

.PHONY: generate/sqlc
generate/sqlc: ## Generate the Go code in internal/db from sql/queries and the migrations
	$(GO) tool sqlc generate

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
