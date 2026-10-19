# Convenience targets (Linux/macOS/WSL). Everything runs in Docker.
COMPOSE = docker compose -f deployments/compose.yaml

.PHONY: up down logs test bench genkey psql

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api worker

test:
	$(COMPOSE) up -d postgres
	$(COMPOSE) --profile test run --rm test

bench:
	$(COMPOSE) --profile test run --rm -e LLM_PROVIDER=deepinfra -e DEEPINFRA_API_KEY test go run ./cmd/aibench -n 100

genkey:
	$(COMPOSE) run --rm --no-deps api /app/admin genkey

psql:
	$(COMPOSE) exec postgres psql -U smsai -d smsai
