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

# The web UI's Node version is pinned once, in web/.nvmrc. A shell without nvm
# loaded still finds nvm's copy of that version.
NODE_VERSION := $(shell cat web/.nvmrc 2>/dev/null)
NVM_DIR ?= $(HOME)/.nvm
NODE_BIN ?= $(shell { \
	ls -1 $(NVM_DIR)/versions/node/v$(NODE_VERSION)/bin/node 2>/dev/null || true; \
	command -v node 2>/dev/null || true; \
	} | head -1)
ifneq (,$(NODE_BIN))
export PATH := $(patsubst %/,%,$(dir $(NODE_BIN))):$(PATH)
endif
NPM := npm --prefix web

# Rejected in a pull request's title, description, and commit messages.
ATTRIBUTION_PATTERN := generated (with|by) \[?claude|co-authored-by:.*(claude|anthropic)|claude\.ai/code/session_

##@ Development Environment

.PHONY: setup
setup: ## Download modules and build the dev tools pinned in go.mod
	$(GO) mod download
	$(GO) build tool
	echo "Ready. Run \`make help\` to see the targets."

.PHONY: watch
watch: ## Rebuild and restart ./build/factory on every Go change (the UI: make serve/web)
	$(GO) tool air \
		--build.cmd "$(MAKE) build/go" \
		--build.entrypoint "$(EXE)" \
		--build.exclude_dir ".github,build,docs,tmp,web,internal/server/public"

# Installs web/node_modules from the lockfile when it changed.
web/node_modules/.installed: web/package.json web/package-lock.json
	if ! command -v node >/dev/null 2>&1; then
		echo "node is not on PATH. Install Node $(NODE_VERSION) (\`nvm install $(NODE_VERSION)\`; web/.nvmrc pins it), then re-run."
		exit 1
	fi
	if [[ "$$(node --version)" != "v$(NODE_VERSION)" ]]; then
		echo "Warning: node is $$(node --version) but web/.nvmrc pins v$(NODE_VERSION). Run \`nvm install $(NODE_VERSION)\` if anything below fails."
	fi
	$(NPM) ci
	touch $@

.PHONY: setup/web
setup/web: web/node_modules/.installed ## Install the web UI's npm dependencies from web/package-lock.json

.PHONY: serve/web
serve/web: web/node_modules/.installed ## Run the web UI's Vite dev server, proxying /api to a running factory serve (FACTORY_URL)
	echo "Proxying /api to $${FACTORY_URL:-http://127.0.0.1:8080}. Start \`factory serve\` there yourself; the UI asks for its FACTORY_API_TOKEN."
	$(NPM) run dev

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
build: build/web build/go ## Build ./build/factory with the web UI embedded

# Vite writes into internal/server/public, which build/go embeds. stub.txt is
# kept so a checkout without a web build still compiles.
.PHONY: build/web
build/web: clean/web web/node_modules/.installed ## Type-check and build the web UI into internal/server/public
	$(NPM) run build

.PHONY: clean/web
clean/web: ## Remove the web build from internal/server/public, keeping stub.txt
	find internal/server/public -mindepth 1 ! -name stub.txt -delete

.PHONY: build/go
build/go: ## Build ./build/factory with the version stamped in and whatever web build is in internal/server/public
	mkdir -p ./build
	$(GO) build -ldflags "$(LDFLAGS)" -o $(EXE) $(MAIN)

IMAGE ?= factory:dev
WORKER_IMAGE ?= factory-worker:dev

.PHONY: build/image
build/image: ## Build the server image (IMAGE=factory:dev) with the version stamped in
	if ! docker info >/dev/null 2>&1; then
		echo "Docker is not running. Start Docker Desktop (or the docker daemon), then run \`make build/image\` again."
		exit 1
	fi
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(IMAGE) -f Dockerfile .

.PHONY: build/image/worker
build/image/worker: ## Build the worker image, the agent dev container (WORKER_IMAGE=factory-worker:dev)
	if ! docker info >/dev/null 2>&1; then
		echo "Docker is not running. Start Docker Desktop (or the docker daemon), then run \`make build/image/worker\` again."
		exit 1
	fi
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) -t $(WORKER_IMAGE) -f Dockerfile.worker .

.PHONY: clean
clean: clean/web ## Remove build output
	rm -rf ./build ./tmp

##@ Test

.PHONY: test
test: ## Run the tests
	$(GO) test ./...

# -count=1 because the database is not an input go's test cache can see.
.PHONY: test/db
test/db: ## Run every test, the database tests included, against Postgres (start it with make serve/postgres)
	FACTORY_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -race -count=1 ./...

# The end-to-end tests run the worker against a real API, Postgres, fakegh, a
# local git origin, and the fake agent. They are behind the e2e build tag.
.PHONY: test/e2e
test/e2e: ## Run the worker's end-to-end tests against Postgres (start it with make serve/postgres)
	FACTORY_DATABASE_URL='$(TEST_DATABASE_URL)' $(GO) test -tags e2e -race -count=1 -run '^TestE2E' ./internal/worker

# The session-boundary tests (#48) as the worker runs: a Linux test binary in the worker image, as uid 1000
# with every capability dropped and tini as PID 1. The sweep kills every process of its user, so it only runs here.
.PHONY: test/contained
test/contained: ## Run the session-boundary tests in WORKER_IMAGE as uid 1000 with no capabilities (needs Docker)
	if ! docker image inspect $(WORKER_IMAGE) >/dev/null 2>&1; then
		echo "No image $(WORKER_IMAGE). Run \`make build/image/worker\` first, or set WORKER_IMAGE to a worker image."
		exit 1
	fi
	mkdir -p build/contained
	GOOS=linux GOARCH="$$(docker version --format '{{.Server.Arch}}')" CGO_ENABLED=0 $(GO) test -c -o build/contained/worker.test ./internal/worker
	docker run --rm --init --cap-drop ALL --security-opt no-new-privileges --user 1000:1000 \
		-e FACTORY_TEST_CONTAINED=1 -v "$(CURDIR)/build/contained:/t:ro" --entrypoint /t/worker.test $(WORKER_IMAGE) \
		-test.v -test.run '^(TestProtectStopsASameUIDRead|TestOthersListsLiveProcesses|TestSweepKillsEveryOtherProcess)$$'

.PHONY: test/golden
test/golden: ## Rewrite the CLI's golden files (cmd/factory/testdata) from the current output; review the diff
	UPDATE_GOLDEN=1 $(GO) test -count=1 ./cmd/factory

.PHONY: test/web
test/web: web/node_modules/.installed ## Run the web UI's component tests
	$(NPM) test

.PHONY: test/race
test/race: ## Run the tests with the race detector
	$(GO) test -race ./...

##@ Code Quality

.PHONY: check
check: check/go check/web test/web ## Check formatting, tidiness, lint, and tests of Go and the web UI without changing files

.PHONY: check/go
check/go: check/format check/tidy check/lint check/sqlc check/migrations test ## The Go half of make check

.PHONY: check/web
check/web: web/node_modules/.installed ## Lint, type-check, and check the formatting of the web UI
	$(NPM) run lint:eslint || { echo "ESLint failed (run \`make fix/web\` for what can be fixed automatically)."; exit 1; }
	$(NPM) run lint:types
	$(NPM) run lint:prettier || { echo "Web files need formatting (run \`make fix/web\`)."; exit 1; }

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

.PHONY: check/chart
check/chart: ## Lint the Helm chart and render it with the default and example values
	if ! command -v helm >/dev/null 2>&1; then
		echo "helm is not installed. Install Helm from https://helm.sh/docs/intro/install/, then re-run this check."
		exit 1
	fi
	helm lint --quiet deploy/chart
	helm lint --quiet deploy/chart -f deploy/chart/values-autobutler.yaml
	helm template factory deploy/chart --namespace factory >/dev/null
	helm template factory deploy/chart --namespace factory -f deploy/chart/values-autobutler.yaml >/dev/null

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

.PHONY: generate/golden
generate/golden: ## Rewrite the planner's golden graphs in internal/plan/testdata after an intended change
	mkdir -p internal/plan/testdata
	FACTORY_UPDATE_GOLDEN=1 $(GO) test ./internal/plan -run TestGolden -count=1

.PHONY: format
format: ## Format Go code (gci, gofumpt, goimports)
	$(GO) tool golangci-lint fmt $(GOLINT_ARGS)

.PHONY: fix
fix: ## Apply lint autofixes, then format
	$(GO) tool golangci-lint run --fix $(GOLINT_ARGS) ./...
	$(MAKE) format

.PHONY: fix/web
fix/web: web/node_modules/.installed ## Apply ESLint autofixes and Prettier to the web UI
	$(NPM) run fix:eslint
	$(NPM) run fix:prettier

.PHONY: tidy
tidy: ## Run go mod tidy
	$(GO) mod tidy

##@ Helpers

.PHONY: help
help: ## Show this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z0-9_\/-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
