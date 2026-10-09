# SPDX-License-Identifier: Apache-2.0
.DEFAULT_GOAL := help
SHELL := /bin/bash

BIN     := bin/aicc
WEB     := web
COMPOSE := deploy/dev/docker-compose.yml

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk -F':.*?## ' '{printf "  %-16s %s\n", $$1, $$2}'

.PHONY: dev-up
dev-up: ## Start development PostgreSQL
	docker compose -f $(COMPOSE) up -d

.PHONY: dev-down
dev-down: ## Stop development PostgreSQL
	docker compose -f $(COMPOSE) down

STACK := deploy/docker-compose.yml

.PHONY: stack-up
stack-up: ## Start the whole product on this machine, seeded
	docker compose -f $(STACK) up -d

.PHONY: stack-config
stack-config: ## Validate the stack's compose files: the base, each overlay, and the stamped release overlay
	@set -euo pipefail; \
	export FS_EXTERNAL_IP=192.0.2.10 FS_LOCAL_IP=192.0.2.10; \
	rel=$$(mktemp -d)/compose.release.yml; \
	trap 'rm -rf "$$(dirname "$$rel")"' EXIT; \
	sed 's/@TAG@/v0.0.0/g' deploy/compose.release.yml.in > "$$rel"; \
	for os in "" deploy/compose.linux.yml deploy/compose.macos.yml; do \
	  for r in "" "$$rel"; do \
	    echo "compose config: $(STACK)$${r:+ + release}$${os:+ + $$os}"; \
	    docker compose -f $(STACK) $${r:+-f "$$r"} $${os:+-f "$$os"} config -q; \
	  done; \
	done

.PHONY: installer-check
installer-check: ## Check the one-line installer: shellcheck, its unit tests, the compose files, a dry release bundle
	@command -v shellcheck >/dev/null 2>&1 || { \
	  echo "installer-check: shellcheck is not installed (apt-get install shellcheck, or brew install shellcheck)" >&2; \
	  exit 1; }
	shellcheck -s sh deploy/install.sh deploy/install_test.sh scripts/release-bundle.sh deploy/postgres/lua-role.sh
	sh deploy/install_test.sh
	$(MAKE) stack-config
	@set -euo pipefail; \
	out=$$(mktemp -d); \
	trap 'rm -rf "$$out"' EXIT; \
	scripts/release-bundle.sh v0.0.0-check "$$out"; \
	cd "$$out"; \
	if command -v sha256sum >/dev/null 2>&1; then sha256sum -c checksums.txt; else shasum -a 256 -c checksums.txt; fi

.PHONY: stack-down
stack-down: ## Stop the stack (add ARGS=-v to discard its data)
	docker compose -f $(STACK) down $(ARGS)

.PHONY: stack-logs
stack-logs: ## Follow the stack's application log
	docker compose -f $(STACK) logs -f aicc

.PHONY: image
image: ## Build the container image (VERSION=v0.1.0 stamps `aicc version`)
	docker build --build-arg VERSION=$(VERSION) -t aicc:$(if $(VERSION),$(VERSION),dev) .

.PHONY: fs-image
fs-image: ## Build the switch image locally for this machine's architecture (LOAD; VERSION defaults to dev)
	VERSION=$(if $(VERSION),$(VERSION),dev) PLATFORM=$(if $(PLATFORM),$(PLATFORM),linux/$(shell uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)) \
	LOAD=1 IMAGE=$(if $(FS_IMAGE),$(FS_IMAGE),aicc-freeswitch) freeswitch/build.sh

.PHONY: fs-push
fs-push: ## Build and push the multi-arch switch image by hand (VERSION=v0.1.0 required; refuses a dirty tree). A release is pushed by the release workflow
	VERSION=$(VERSION) freeswitch/build.sh

# The generated files name the sqlc that wrote them, so any other version
# rewrites every header. CI installs exactly this one (sqlc-install).
SQLC_VERSION := 1.31.1

.PHONY: generate
generate: ## Regenerate sqlc query code
	sqlc generate

.PHONY: sqlc-install
sqlc-install: ## Install the pinned sqlc (needs cgo)
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@v$(SQLC_VERSION)

.PHONY: sqlc-check
sqlc-check: ## CI gate: committed sqlc code matches the migrations and queries
	@test "$$(sqlc version)" = "v$(SQLC_VERSION)" \
		|| { echo "sqlc-check: need sqlc v$(SQLC_VERSION), have $$(sqlc version 2>/dev/null || echo none) (make sqlc-install)"; exit 1; }
	sqlc generate
	git diff --exit-code -- internal/store/queries
	@untracked=$$(git ls-files --others --exclude-standard -- internal/store/queries); \
		test -z "$$untracked" || { echo "$$untracked"; echo 'untracked generated files'; exit 1; }

REDOCLY := $(WEB)/node_modules/.bin/redocly

.PHONY: api-lint
api-lint: ## Lint the API contract (docs/openapi.json)
	$(REDOCLY) lint docs/openapi.json

.PHONY: api-generate
api-generate: ## Regenerate Go + TS API code from the contract
	scripts/api-generate.sh

.PHONY: api-check
api-check: api-lint ## CI gate: contract lints and committed generated code matches it
	scripts/api-generate.sh
	git diff --exit-code -- internal/api web/src/generated
	@test -z "$$(git status --porcelain -- internal/api web/src/generated)" \
		|| { git status --short -- internal/api web/src/generated; echo 'untracked generated files'; exit 1; }

BASE ?= main
# Breaking changes that were reviewed and accepted, one pinned line each.
API_BREAKING_IGNORE ?= .oasdiff-breaking-ignore.txt

.PHONY: api-breaking
api-breaking: ## Fail on undeclared breaking API changes vs BASE (default main)
	@base_spec=$$(mktemp); \
	if git show $(BASE):docs/openapi.json > $$base_spec 2>/dev/null; then \
		go tool oasdiff breaking --fail-on ERR --err-ignore $(API_BREAKING_IGNORE) $$base_spec docs/openapi.json; status=$$?; \
	else \
		echo "no contract on $(BASE); nothing to compare"; status=0; \
	fi; \
	rm -f $$base_spec; exit $$status

.PHONY: build
build: web-build ## Build the single executable with the SPA embedded
	go build -o $(BIN) ./cmd/aicc

.PHONY: run
run: ## Run the server (expects dev-up and a built SPA, or use the Vite dev server)
	go run ./cmd/aicc

# Without AICC_TEST_DATABASE_URL every test that calls scratchDB skips and the
# run stays green (see the comment in .github/workflows/ci.yml), so say so
# after the suite, where it is not buried, whether the suite passed or failed.
TEST_DATABASE_URL := postgres://aicc:aicc@127.0.0.1:5432/aicc?sslmode=disable

.PHONY: test
test: ## Run Go (with -race) and frontend tests, as CI does
	@status=0; \
	( set -x; go test -race ./... && cd $(WEB) && npm run test ) || status=$$?; \
	if [ -z "$${AICC_TEST_DATABASE_URL:-}" ]; then \
	  printf '%s\n' '' \
	    '************************************************************************' \
	    'WARNING: AICC_TEST_DATABASE_URL is unset, so every database test was' \
	    'SKIPPED: migrations, store, and much of seed and httpapi. CI runs them.' \
	    'To run them here, start PostgreSQL (make dev-up), then:' \
	    "  AICC_TEST_DATABASE_URL='$(TEST_DATABASE_URL)' make test" \
	    '************************************************************************' >&2; \
	fi; \
	exit $$status

.PHONY: lint
lint: ## Vet Go code and lint the frontend
	go vet ./...
	gofmt -l . | grep -v node_modules && exit 1 || true
	cd $(WEB) && npx oxlint .

.PHONY: web-install
web-install: ## Install frontend dependencies
	cd $(WEB) && npm install

.PHONY: web-dev
web-dev: ## Run the Vite dev server (proxies /api to :8080)
	cd $(WEB) && npm run dev

.PHONY: web-build
web-build: ## Build the SPA into web/dist for embedding
	cd $(WEB) && npm run build

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN) $(WEB)/dist/assets $(WEB)/dist/index.html
