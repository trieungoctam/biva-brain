COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: up down clean logs ps migrate smoke test test-go test-py lint fmt

up:            ## dựng và chạy toàn bộ môi trường local
	$(COMPOSE) up -d --build

down:          ## dừng (giữ dữ liệu)
	$(COMPOSE) down

clean:         ## dừng và xoá volume
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f --tail=100

ps:
	$(COMPOSE) ps

migrate:       ## chạy migration lên Postgres của compose
	$(COMPOSE) run --rm migrate

smoke:         ## make up + kiểm luồng MCP → queue → worker (+ TEI); SMOKE_SKIP_TEI=1 để bỏ TEI
	deploy/smoke.sh

test: test-go test-py

test-go:       ## đặt BIVA_TEST_DATABASE_URL để chạy cả test migration với Postgres thật
	cd go && go vet ./... && go test -p 1 ./...   # -p 1: các test dùng chung một DB

test-py:
	cd python && uv run pytest -q

lint:
	cd go && test -z "$$(gofmt -l .)" && go vet ./... && go run ./cmd/brain-api kb check
	cd python && uv run ruff check . && uv run ruff format --check .

fmt:
	cd go && gofmt -w .
	cd python && uv run ruff format . && uv run ruff check --fix .
