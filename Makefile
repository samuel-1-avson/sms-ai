# Convenience targets (Linux/macOS/WSL). Everything runs in Docker.
COMPOSE = docker compose -f deployments/compose.yaml

.PHONY: up down logs test genkey pair tunnel psql

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f api worker

test:
	$(COMPOSE) up -d postgres
	$(COMPOSE) --profile test run --rm test

genkey:
	$(COMPOSE) run --rm --no-deps api node dist/admin.js genkey

# Print the QR code that pairs the gateway phone (needs GATEWAY_PUBLIC_URL and GATEWAY_DEVICE_SECRET in .env).
pair:
	$(COMPOSE) run --rm --no-deps api node dist/admin.js gateway-pair

# Public HTTPS URL for the local API, for pairing a real phone with a laptop stack.
tunnel:
	$(COMPOSE) --profile tunnel up -d tunnel
	sleep 5; $(COMPOSE) logs tunnel | grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' | tail -1

psql:
	$(COMPOSE) exec postgres psql -U smsai -d smsai
