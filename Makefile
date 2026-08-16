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
	docker-compose -f $(COMPOSE) up -d

.PHONY: dev-down
dev-down: ## Stop development PostgreSQL
	docker-compose -f $(COMPOSE) down

DEMO := deploy/demo/docker-compose.yml

.PHONY: demo-up
demo-up: ## Start the full demo stack (app + FreeSWITCH + PostgreSQL, seeded)
	docker-compose -f $(DEMO) up -d

.PHONY: demo-down
demo-down: ## Stop the demo stack (add ARGS=-v to discard its data)
	docker-compose -f $(DEMO) down $(ARGS)

.PHONY: demo-logs
demo-logs: ## Follow the demo application log
	docker-compose -f $(DEMO) logs -f aicc

.PHONY: image
image: ## Build the container image (VERSION=v0.1.0 stamps `aicc version`)
	docker build --build-arg VERSION=$(VERSION) -t aicc:$(if $(VERSION),$(VERSION),dev) .

.PHONY: generate
generate: ## Regenerate sqlc query code
	sqlc generate

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

.PHONY: api-breaking
api-breaking: ## Fail on undeclared breaking API changes vs BASE (default main)
	@base_spec=$$(mktemp); \
	if git show $(BASE):docs/openapi.json > $$base_spec 2>/dev/null; then \
		go tool oasdiff breaking --fail-on ERR $$base_spec docs/openapi.json; status=$$?; \
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

.PHONY: test
test: ## Run Go tests
	go test ./...

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
