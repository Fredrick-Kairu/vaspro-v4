SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

COMPOSE := docker compose -f deploy/compose/docker-compose.yml
GO_PKGS := $(shell go list ./... 2>/dev/null)

.PHONY: up down reset gen gen-check migrate lint test test-int test-contract test-e2e check load run new-service

up:
	$(COMPOSE) up -d --wait

down:
	$(COMPOSE) down

reset:
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait

# Runs every generator whose input already exists. Each step is a no-op,
# not a failure, until the task that introduces its input lands. Every
# generator's output must be named *_gen.<ext> — gen-check's diff is
# scoped to exactly that pattern, so it can tell "generated code is
# stale" apart from "something else in the tree changed" regardless of
# what else is mid-edit (see Makefile history: go.mod, non-generated
# test paths, and plain doc edits all tripped up an earlier, unscoped
# version of this check).
gen:
	@if [ -f sqlc.yaml ]; then sqlc generate; else echo "gen: no sqlc.yaml yet, skipping sqlc"; fi
	@if [ -f buf.yaml ]; then buf generate; else echo "gen: no buf.yaml yet, skipping buf"; fi
	@if ls docs/api/*.yaml >/dev/null 2>&1; then \
		for f in docs/api/*.yaml; do oapi-codegen -config "$$f.oapi-codegen.yaml" "$$f"; done; \
	else echo "gen: no OpenAPI specs yet, skipping oapi-codegen"; fi
	@if [ -f docs/errors/catalogue.yaml ] && [ -d tools/errcodes ]; then go run ./tools/errcodes; \
	else echo "gen: no error catalogue or generator yet, skipping"; fi

gen-check: gen
	@git diff --exit-code -- ':(glob)**/*_gen.*' || \
		(echo "gen-check: generated code is out of date; run make gen and commit the result" && exit 1)

migrate:
	@for dir in migrations/*/; do \
		schema=$$(basename "$$dir"); \
		echo "migrate: $$schema"; \
		goose -dir "$$dir" postgres "$$DATABASE_URL_$$schema" up; \
	done

lint:
	@if [ -z "$(GO_PKGS)" ]; then echo "lint: no Go packages yet, skipping golangci-lint"; \
	else golangci-lint run ./...; fi
	@if command -v gitleaks >/dev/null 2>&1; then gitleaks detect --no-banner; \
	else echo "lint: gitleaks not installed, skipping secret scan"; fi

test:
	@if [ -z "$(GO_PKGS)" ]; then echo "test: no Go packages yet, skipping"; \
	else go test ./...; fi

test-int:
	@if [ -z "$(GO_PKGS)" ]; then echo "test-int: no Go packages yet, skipping"; \
	else go test -tags=integration ./...; fi

test-contract:
	@if [ -z "$$(go list ./test/contract/... 2>/dev/null)" ]; then echo "test-contract: no packages under test/contract yet, skipping"; \
	else go test -tags=contract ./test/contract/...; fi

test-e2e:
	@if [ -z "$$(go list ./test/e2e/... 2>/dev/null)" ]; then echo "test-e2e: no packages under test/e2e yet, skipping"; \
	else go test -tags=e2e ./test/e2e/...; fi

check: gen-check lint test test-int test-contract

load:
	k6 run test/load/$(SCENARIO)

run:
ifdef SVC
	go run ./cmd/$(SVC)
else
	go run ./cmd/vaspro-all
endif

new-service:
	go run ./tools/scaffold -name $(NAME)
