#!/usr/bin/env bash
# 触发服务端 VACUUM INTO 在线热备，把数据卷内的备份拉到宿主机 ./backups，并按天滚动清理。
# 建议加入宿主机 crontab：0 3 * * * cd /opt/family-workspace && ./scripts/backup.sh >> /var/log/fw-backup.log 2>&1
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "缺少 .env，请先执行: cp .env.example .env 并修改口令" >&2
  exit 1
fi
set -a
# shellcheck disable=SC1091
source .env
set +a

: "${FW_AUTH_PASSWORD:?请在 .env 设置 FW_AUTH_PASSWORD}"
PORT="${FW_PORT:-8080}"
KEEP_DAYS="${FW_BACKUP_KEEP_DAYS:-7}"

# 容器运行时与 Makefile 保持一致；Podman 机器上可 COMPOSE="podman compose" ./scripts/backup.sh
COMPOSE="${COMPOSE:-docker compose}"
read -ra COMPOSE_CMD <<< "${COMPOSE}"

echo "[1/3] 触发在线热备 ..."
curl -fsS -X POST \
  -H "X-FW-Token: ${FW_AUTH_PASSWORD}" \
  "http://127.0.0.1:${PORT}/api/v1/admin/backup" >/dev/null

mkdir -p ./backups
echo "[2/3] 从数据卷拉取备份文件 ..."
"${COMPOSE_CMD[@]}" cp family:/data/backup/. ./backups/ >/dev/null

echo "[3/3] 清理 ${KEEP_DAYS} 天前的备份 ..."
find ./backups -type f -name 'family-*.db' -mtime "+${KEEP_DAYS}" -delete

echo "完成，当前备份："
ls -lh ./backups/family-*.db 2>/dev/null || echo "（无）"
