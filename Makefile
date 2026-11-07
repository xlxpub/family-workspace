SHELL := /bin/bash
GO ?= go
# 锁定工具链，避免 go 命令自动升级到高于 go.mod 的版本
export GOTOOLCHAIN := local

.PHONY: help vet test build build-arm64 run image up down logs ps backup clean

help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

vet: ## 静态检查
	$(GO) vet ./...

test: vet ## 运行全部集成测试
	$(GO) test ./... -count=1

build: ## 编译当前平台到 bin/family-server
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w" -o bin/family-server .

build-arm64: ## 交叉编译 ARM64（NAS / 树莓派）
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="-s -w" -o bin/family-server-linux-arm64 .

run: ## 本地直接运行 http://localhost:8080
	$(GO) run .

image: ## 构建 Docker 镜像
	docker compose build

up: ## 构建并后台启动（需先准备 .env）
	docker compose up -d --build

down: ## 停止并移除容器（数据卷保留）
	docker compose down

logs: ## 跟踪日志
	docker compose logs -f --tail=100

ps: ## 查看容器与健康状态
	docker compose ps

backup: ## 触发在线热备并拉取到 ./backups
	./scripts/backup.sh

clean: ## 清理编译产物
	rm -rf bin
