SHELL := /bin/bash
GO ?= go
# 容器运行时：默认 docker compose。
# CentOS / RHEL 上装的是 Podman 时：make up COMPOSE="podman compose"
# （alias docker=podman 只对交互式 shell 生效，make 的 recipe 不会继承别名）
COMPOSE ?= docker compose
# 一并传给 recipe（scripts/backup.sh 读同名环境变量）
export COMPOSE
# 锁定工具链，避免 go 命令自动升级到高于 go.mod 的版本
export GOTOOLCHAIN := local

.PHONY: help vet test build build-arm64 webhook-build webhook-up webhook-down webhook-logs run local image up down logs ps backup clean

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

vet: ## 静态检查
	$(GO) vet ./...

test: vet ## 运行全部集成测试
	$(GO) test ./... -count=1

build: ## 编译当前平台到 bin/family-server
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/family-server .

webhook-build: ## 编译宿主机 GitHub webhook 服务
	CGO_ENABLED=0 $(GO) build -trimpath -o bin/github-webhook ./cmd/github-webhook

# podman-compose 1.0.6 不能解析嵌套变量，因此不用 compose 文件启动。
# docker 在 OpenCloudOS 上是 Podman 包装，这些命令同样适用。
webhook-up: ## 启动独立的 8091 webhook 服务（不会被 make up 重启）
	@test -f deploy/github-webhook.env || { echo "请先复制 deploy/github-webhook.env.example 为 deploy/github-webhook.env"; exit 1; }
	docker build -f Dockerfile.webhook -t family-webhook:latest .
	-docker rm -f family-webhook
	docker run -d --name family-webhook --restart unless-stopped \
		--network host --pid host --privileged \
		--env-file deploy/github-webhook.env \
		-e FW_WEBHOOK_DIR="$(CURDIR)" \
		-e FW_WEBHOOK_ADDR=127.0.0.1:8091 \
		-e FW_WEBHOOK_NSENTER=1 \
		-e GIT_TERMINAL_PROMPT=0 \
		family-webhook:latest

webhook-down: ## 停止 8091 webhook 服务
	docker rm -f family-webhook

webhook-logs: ## 查看 8091 webhook 服务日志
	docker logs -f --tail=100 family-webhook

build-arm64: ## 交叉编译 ARM64（NAS / 树莓派）
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o bin/family-server-linux-arm64 .

run: ## 本地直接运行（需自行 export FW_AUTH_PASSWORD）http://localhost:8080
	$(GO) run .

local: ## 本机一键启动（macOS / Linux，自动读取 .env，不依赖 Docker）
	./scripts/start-local.sh

image: ## 构建 Docker 镜像
	$(COMPOSE) build

up: ## 构建并后台启动（需先准备 .env；Podman 用 COMPOSE="podman compose"）
	$(COMPOSE) up -d --build

down: ## 停止并移除容器（数据卷保留）
	$(COMPOSE) down

logs: ## 跟踪日志
	$(COMPOSE) logs -f --tail=100

ps: ## 查看容器与健康状态
	$(COMPOSE) ps

backup: ## 触发在线热备并拉取到 ./backups
	./scripts/backup.sh

clean: ## 清理编译产物
	rm -rf bin
