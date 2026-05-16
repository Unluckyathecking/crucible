.PHONY: help dev down logs worker gateway dashboard test tidy smoke-test-new-tool

help:
	@echo "Crucible — local development targets:"
	@echo "  make dev       Bring up postgres + redis + worker + gateway (docker compose)"
	@echo "  make down      Tear down the docker stack (keeps volumes)"
	@echo "  make logs      Tail logs from all services"
	@echo "  make worker    Run the worker stub directly via go run on :8081"
	@echo "  make gateway   Run the gateway directly via go run on :8080"
	@echo "  make test      Run all Go tests"
	@echo "  make tidy      go mod tidy across all modules"

dev:
	docker compose up -d --build
	@echo
	@echo "Stack up. Smoke test:"
	@echo "  curl -s -X POST localhost:8080/v1/echo \\"
	@echo "    -H 'content-type: application/json' \\"
	@echo "    -d '{\"x\":\"hi\"}' | jq"

down:
	docker compose down

logs:
	docker compose logs -f

worker:
	go run ./workers/stubs/go

gateway:
	go run ./gateway/cmd/gateway

test:
	cd workers/sdk-go && go test ./...
	cd gateway && go test ./...
	cd workers/stubs/go && go test ./...

tidy:
	cd workers/sdk-go && go mod tidy
	cd workers/stubs/go && go mod tidy
	cd gateway && go mod tidy

dashboard:
	cd dashboard && pnpm dev

smoke-test-new-tool: ## Run the same canonical script that CI runs on every PR
	bash scripts/smoke-new-tool.sh
