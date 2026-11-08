#!/usr/bin/env bash
# 本机开发启动（macOS / Linux），不依赖 Docker。
# 关键：main.go 只读环境变量、不读 .env；本脚本负责把配置注入环境。
#
# 用法（口令优先级：环境变量 > 第一个参数 > .env）：
#   FW_AUTH_PASSWORD='你的强口令' ./scripts/start-local.sh
#   ./scripts/start-local.sh '你的强口令'
#   cp .env.example .env && $EDITOR .env && ./scripts/start-local.sh
#
# 访问 http://localhost:8080 ，管理员账号 admin / 该口令。
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

if ! command -v go >/dev/null 2>&1; then
  echo "✗ 未找到 go。macOS 安装：brew install go（或官网 pkg）" >&2
  exit 1
fi

# 1) 命令行参数作为口令
if [ -n "${1:-}" ]; then export FW_AUTH_PASSWORD="$1"; fi

# 2) 从 .env 补白名单变量：逐行解析而非 source，避免口令里的 $ / 空格 / 引号被 shell 解释
if [ -f .env ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|\#*) continue ;; esac
    k="${line%%=*}"; v="${line#*=}"
    k="$(printf %s "$k" | tr -d '[:space:]')"
    case "$k" in
      FW_AUTH_PASSWORD|FW_DB|FW_ADDR|FW_COOKIE_SECURE|TZ|FW_AI_BASE_URL|FW_AI_MODEL|FW_AI_API_KEY) ;;
      FW_AI_*|FW_MODEL*)
        # 典型事故：把 FW_AI_MODEL 写成 FW_AI_MODE / FW_AI_API_KE，变量被静默丢弃后回落到默认模型
        echo "✗ .env 中的 $k 不是有效变量名，已忽略；只认 FW_AI_BASE_URL / FW_AI_MODEL / FW_AI_API_KEY" >&2
        continue ;;
      *) continue ;;
    esac
    case "$v" in
      \"*\") v="${v#\"}"; v="${v%\"}" ;;
      \'*\') v="${v#\'}"; v="${v%\'}" ;;
    esac
    if [ -z "$(printenv "$k" 2>/dev/null || true)" ]; then export "$k=$v"; fi
  done < .env
fi

# 3) 口令必须有（留空会进入安装模式，不适合日常使用）
if [ -z "${FW_AUTH_PASSWORD:-}" ]; then
  cat >&2 <<'TIP'
✗ 未设置 FW_AUTH_PASSWORD（初始管理员口令 + 备份 API Token）

  推荐：FW_AUTH_PASSWORD='换成你的强口令' ./scripts/start-local.sh
  或：  cp .env.example .env && $EDITOR .env  改好口令后重新运行本脚本

  注意：.env 只被本脚本和 Docker Compose 读取，直接 `go run .` / `make run` 不会读它。
TIP
  exit 1
fi

mkdir -p data
export FW_DB="${FW_DB:-./data/family.db}"
export FW_ADDR="${FW_ADDR:-:8080}"
export FW_COOKIE_SECURE="${FW_COOKIE_SECURE:-0}"
export GOTOOLCHAIN=local

port="${FW_ADDR##*:}"
echo "→ 家庭工作台启动中：http://localhost:${port}"
echo "  数据库：${FW_DB}（相对本仓库根目录）"
echo "  管理员：admin / FW_AUTH_PASSWORD（首次登录后请在「用户与权限」改密）"
echo "  停止：Ctrl-C；数据在 data/ 中保留"
exec go run .
