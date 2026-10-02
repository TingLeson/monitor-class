# ============================================================================
# ClassWatch — 本地开发入口
#
# 常用：
#   make init          初始化（生成 .env、安装依赖）
#   make dev           一键启动：容器（postgres/redis/livekit/api）+ 三个前端
#   make doctor        检查本地环境是否健康
#   make test lint     质量检查
#   make down          停止容器
#
# 说明：`-include .env` 让 make 变量与 .env 保持一致，并用裸 `export`
# 把它们导出给 recipe（这样 `make dev-api` 可以在宿主直接跑 Go 服务）。
# docker compose 自己也会读 .env，两者不冲突。
# ============================================================================

SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE ?= docker compose
API_DIR := services/api
ENV_FILE := .env

-include $(ENV_FILE)
export

.PHONY: help init env install up down restart ps logs dev dev-api dev-web doctor \
        migrate-up migrate-status create-admin create-user reset-password list-users \
        db-shell sql redis-cli \
        build build-api build-web image \
        test test-api test-web test-integration \
        lint lint-api lint-web fmt fmt-api fmt-web \
        ci clean nuke

# ---------------------------------------------------------------- 帮助

help: ## 显示所有可用目标
	@echo "ClassWatch 本地开发命令："
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------- 初始化

init: env install ## 初始化本地开发环境（.env + 依赖）

env: ## 按需从 .env.example 生成 .env
	@if [ ! -f $(ENV_FILE) ]; then \
		cp .env.example $(ENV_FILE); \
		echo "已生成 $(ENV_FILE)（本地开发占位密钥，禁止用于生产）"; \
	fi

install: ## 安装前端与后端依赖
	pnpm install
	cd $(API_DIR) && go mod download

# ---------------------------------------------------------------- 容器环境

up: env ## 启动 postgres/redis/livekit/api 容器
	$(COMPOSE) up -d --build
	@echo "API:      http://localhost:8080/healthz"
	@echo "LiveKit:  ws://localhost:7880"

down: ## 停止容器（保留数据卷）
	$(COMPOSE) down

restart: ## 重建并重启容器
	$(COMPOSE) up -d --build --force-recreate

ps: ## 查看容器状态
	$(COMPOSE) ps

logs: ## 跟踪容器日志（make logs S=api 可只看某个服务）
	$(COMPOSE) logs -f $(S)

# ---------------------------------------------------------------- 开发

dev: up ## 一键启动：容器 + 三个前端 dev server（前台，Ctrl-C 退出前端）
	@echo ""
	@echo "Student: http://localhost:5173/student/login"
	@echo "Teacher: http://localhost:5174/teacher/login"
	@echo "Admin:   http://localhost:5175/admin/login"
	@echo "（容器仍在后台运行，使用 make down 停止）"
	@echo ""
	pnpm dev

dev-api: env ## 在宿主机直接运行 Go API（支持热重启需自行用 air）
	cd $(API_DIR) && go run ./cmd/api

dev-web: ## 只启动三个前端 dev server
	pnpm dev

doctor: env ## 检查本地环境健康状态（API + 依赖连通性）
	@printf '%-34s' "GET /healthz"; curl -s -o /tmp/cw_healthz.json -w '%{http_code} ' http://localhost:$${API_PORT:-8090}/healthz; cat /tmp/cw_healthz.json; echo
	@printf '%-34s' "GET /readyz"; curl -s -o /tmp/cw_readyz.json -w '%{http_code} ' http://localhost:$${API_PORT:-8090}/readyz; cat /tmp/cw_readyz.json; echo
	@printf '%-34s' "LiveKit 7880"; curl -s -o /dev/null -w '%{http_code}\n' http://localhost:7880/ || true
	@$(COMPOSE) ps

# ---------------------------------------------------------------- 数据库

migrate-up: env ## 执行数据库 migration（versioned SQL，幂等）
	$(COMPOSE) run --rm migrate up

migrate-status: env ## 查看已应用的 migration
	$(COMPOSE) run --rm migrate status

create-admin: env ## 交互式创建初始管理员（密码不回显、不进 shell 历史）
	@read -r -p "account: " account; \
	 read -r -p "display name: " name; \
	 read -r -s -p "password: " password; echo; \
	 printf '%s' "$$password" | $(COMPOSE) run --rm -T adminctl \
		create-user --account "$$account" --display-name "$$name" --role ADMIN --password-stdin

create-user: env ## 创建用户：make create-user ROLE=TEACHER|STUDENT（交互式，STUDENT 无需密码）
	@read -r -p "account: " account; \
	 read -r -p "display name: " name; \
	 if [ "$(ROLE)" = "STUDENT" ]; then \
		$(COMPOSE) run --rm -T adminctl create-user --account "$$account" --display-name "$$name" --role STUDENT; \
	 else \
		read -r -s -p "password: " password; echo; \
		printf '%s' "$$password" | $(COMPOSE) run --rm -T adminctl \
			create-user --account "$$account" --display-name "$$name" --role "$${ROLE:-TEACHER}" --password-stdin; \
	 fi

reset-password: env ## 重置密码：make reset-password ACCOUNT=teacher001
	@read -r -s -p "new password: " password; echo; \
	 printf '%s' "$$password" | $(COMPOSE) run --rm -T adminctl \
		reset-password --account "$(ACCOUNT)" --password-stdin

list-users: env ## 列出账号
	$(COMPOSE) run --rm -T adminctl list-users $(if $(ROLE),--role $(ROLE),)

db-shell: ## 进入 postgres psql
	$(COMPOSE) exec postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB)

sql: ## 执行一段 SQL：make sql Q="select 1"
	$(COMPOSE) exec -T postgres psql -U $(POSTGRES_USER) -d $(POSTGRES_DB) -c "$(Q)"

redis-cli: ## 进入 redis-cli
	$(COMPOSE) exec redis redis-cli

# ---------------------------------------------------------------- 构建

build: build-api build-web ## 构建后端与三个前端

build-api: ## 编译 Go API
	cd $(API_DIR) && go build ./...

build-web: ## 构建三个前端产物
	pnpm build

image: ## 构建 API 容器镜像
	$(COMPOSE) build api

# ---------------------------------------------------------------- 测试

test: test-api test-web ## 运行全部测试

test-api: ## 运行 Go 单元测试
	cd $(API_DIR) && go test ./...

test-integration: env ## 运行需要真实 PostgreSQL 的集成测试
	@$(COMPOSE) exec -T postgres psql -U $(POSTGRES_USER) -d postgres -tAc \
		"SELECT 1 FROM pg_database WHERE datname = 'classwatch_test'" | grep -q 1 \
		|| $(COMPOSE) exec -T postgres createdb -U $(POSTGRES_USER) classwatch_test
	@# DSN 在这里显式拼装：集成测试必须打真实的 compose PostgreSQL，而不是某个本地残留实例。
	@# Phase 1 起集成测试分布在 user / auth / httpapi / ratelimit / cmd 等多个包，因此跑全量。
	@# -p 1：这些包共用同一个 classwatch_test 库，包级并行会互相干扰（例如某个用例临时
	@# 调整 ACTIVE 管理员集合时，另一个包同时在数管理员）。串行换来的是稳定，代价只有十几秒。
	cd $(API_DIR) && TEST_DATABASE_URL="postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@localhost:$(POSTGRES_PORT)/classwatch_test?sslmode=disable" \
		TEST_REDIS_ADDR="localhost:$(REDIS_PORT)" \
		go test -p 1 ./... -count=1

test-web: ## 运行前端单元测试
	pnpm test

# ---------------------------------------------------------------- 质量

lint: lint-api lint-web ## 运行全部静态检查

lint-api: ## Go 格式与 vet 检查
	@cd $(API_DIR) && test -z "$$(gofmt -l .)" || { echo "gofmt 需要格式化以下文件："; cd $(API_DIR) && gofmt -l .; exit 1; }
	cd $(API_DIR) && go vet ./...

lint-web: ## 前端 ESLint + Prettier 检查
	pnpm lint
	pnpm format:check

fmt: fmt-api fmt-web ## 格式化全部代码

fmt-api: ## gofmt
	cd $(API_DIR) && gofmt -w .

fmt-web: ## Prettier
	pnpm format

ci: ## 在本地执行与 CI 等价的检查
	pnpm install --frozen-lockfile
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) build

# ---------------------------------------------------------------- 清理

clean: ## 清理构建产物
	rm -rf apps/*/dist packages/*/dist coverage
	cd $(API_DIR) && rm -rf bin && go clean -testcache

nuke: ## 停止容器并删除数据卷（会清空本地数据库）
	$(COMPOSE) down -v
