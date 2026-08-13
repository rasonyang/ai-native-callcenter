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

.PHONY: generate
generate: ## Regenerate sqlc query code
	sqlc generate

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
