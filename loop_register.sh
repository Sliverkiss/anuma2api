#!/usr/bin/env bash
# Anuma AI 自动注册循环 wrapper (可配 systemd 定时任务)
# 每轮注册 1 个账号, 完成后 sleep REG_INTERVAL 秒再继续 (默认 15s)
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR" || exit 1
export DISPLAY="${DISPLAY:-:99}"
INTERVAL="${REG_INTERVAL:-15}"

echo "[$(date '+%F %T')] anuma-register loop started, interval=${INTERVAL}s"

while true; do
  echo "[$(date '+%F %T')] === iteration start ==="
  "${PYTHON:-python3}" "$SCRIPT_DIR/register.py" --count 1
  code=$?
  echo "[$(date '+%F %T')] register.py exit=$code; sleep ${INTERVAL}s"
  sleep "${INTERVAL}"
done
