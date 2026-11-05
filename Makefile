SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

COMPOSE := docker compose -f deploy/compose/docker-compose.yml
# The dev environment (docs/runbooks/dev-environment.md).
COMPOSE_DEV := docker compose -f deploy/compose/docker-compose.dev.yml --env-file deploy/compose/.env
GO_PKGS := $(shell go list ./... 2>/dev/null)

.PHONY: up down reset gen gen-check migrate lint test test-int test-contract test-e2e check load run new-service dev-up dev-down dev-reset dev-logs dev-kyc dev-staff

up:
	$(COMPOSE) up -d --wait

down:
	$(COMPOSE) down

reset:
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait

dev-up:
	$(COMPOSE_DEV) up -d --build --wait

dev-down:
	$(COMPOSE_DEV) down

dev-reset:
	$(COMPOSE_DEV) down -v
	$(COMPOSE_DEV) up -d --build --wait

dev-logs:
	$(COMPOSE_DEV) logs -f $(SVC)

# make dev-kyc ID=<check id> STATUS=passed|review|failed|expired (no ID: list)
dev-kyc:
	@if [ -z "$${ID:-}" ]; then $(COMPOSE_DEV) exec kyc-vendor kycvendor list; \
	else $(COMPOSE_DEV) exec kyc-vendor kycvendor decide "$(ID)" "$(STATUS)"; fi

# make dev-staff EMAIL=<user's email> BY=<operator>
dev-staff:
	$(COMPOSE_DEV) run --rm vaspro identity grant-staff -by "$(BY)" "$(EMAIL)"

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
	@if [ -f buf.yaml ]; then buf lint && buf generate; else echo "gen: no buf.yaml yet, skipping buf"; fi
	@if ls docs/api/*.yaml >/dev/null 2>&1; then \
		for f in docs/api/*.yaml; do \
			case "$$f" in *.oapi-codegen.yaml) continue;; esac; \
			oapi-codegen -config "$$f.oapi-codegen.yaml" "$$f"; \
		done; \
	else echo "gen: no OpenAPI specs yet, skipping oapi-codegen"; fi
	@if [ -f docs/errors/catalogue.yaml ] && [ -d tools/errcodes ]; then go run ./tools/errcodes; \
	else echo "gen: no error catalogue or generator yet, skipping"; fi
	@go run ./tools/apidocs

# protoc plugins can't name their output *_gen, so their *.pb.go files
# are checked too (ADR 0004, amended in task 2.2a), and so is the API
# reference tools/apidocs writes.
gen-check: gen
	@git diff --exit-code -- ':(glob)**/*_gen.*' ':(glob)**/*.pb.go' docs/api/API.md || \
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
	@if command -v gitleaks >/dev/null 2>&1; then gitleaks detect --no-banner --redact --verbose; \
	else echo "lint: gitleaks not installed, skipping secret scan"; fi

test:
	@if [ -z "$(GO_PKGS)" ]; then echo "test: no Go packages yet, skipping"; \
	else go test ./...; fi

# Integration packages each start their own containers (Postgres, NATS,
# Redis, RabbitMQ, SeaweedFS, ...). Go runs one package per CPU by
# default, which on a 10-core machine meant ~300 containers at once on
# an 8 GB Docker VM and containers that never became ready. Capping the
# packages run at once keeps Docker within its means; override with
# `make test-int INTEGRATION_PARALLELISM=n`.
INTEGRATION_PARALLELISM ?= 4

test-int:
	@if [ -z "$(GO_PKGS)" ]; then echo "test-int: no Go packages yet, skipping"; \
	else go test -tags=integration -p $(INTEGRATION_PARALLELISM) ./...; fi

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
