#!/bin/bash
# pool_keeper.sh — Anuma 号池自动补号
# 检查网关总 credits，低于阈值时自动注册新账号补池
set -u

GATEWAY_URL="${ANUMA_GATEWAY_URL:-http://127.0.0.1:17869}"
GATEWAY_TOKEN="${ANUMA_GATEWAY_TOKEN:-anuma2api2026}"
THRESHOLD="${POOL_THRESHOLD:-500}"
TARGET="${POOL_TARGET:-700}"
IMAGE="${POOL_IMAGE:-anuma2api/register:browser-ready}"
APP_DIR="${POOL_APP_DIR:-/tmp/anuma2api}"
LOG="$APP_DIR/pool_keeper.log"
LOCK="$APP_DIR/pool_keeper.lock"
CONTAINER_NAME="anuma-register"

log() { echo "[$(date '+%F %T')] $*" | tee -a "$LOG"; }

# 防重入
if [ -f "$LOCK" ]; then
  pid=$(cat "$LOCK")
  if kill -0 "$pid" 2>/dev/null; then
    log "已有补号进程在跑 (pid=$pid)，跳过"
    exit 0
  fi
  rm -f "$LOCK"
fi
echo $$ > "$LOCK"
trap 'rm -f "$LOCK"' EXIT

# 查询当前总 credits
fetch_total() {
  curl -s --max-time 10 -H "Authorization: Bearer $GATEWAY_TOKEN" \
    "$GATEWAY_URL/api/accounts" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    accts = data.get('data', data) if isinstance(data, dict) else data
    if isinstance(accts, dict):
        accts = accts.get('accounts', [])
    total = sum(int(a.get('available_credits', a.get('credits', 0)) or 0) for a in accts)
    print(total)
except Exception:
    print(-1)
" 2>/dev/null
}

TOTAL=$(fetch_total)
if [ "$TOTAL" -lt 0 ]; then
  log "网关查询失败，跳过本次"
  exit 1
fi

log "当前总 credits: $TOTAL (阈值 $THRESHOLD, 目标 $TARGET)"

if [ "$TOTAL" -ge "$THRESHOLD" ]; then
  log "池子充足，无需补号"
  exit 0
fi

# 需补账号数（每账号 ~100 credits，向上取整）
NEED=$(( (TARGET - TOTAL + 99) / 100 ))
[ "$NEED" -lt 1 ] && NEED=1
[ "$NEED" -gt 5 ] && NEED=5  # 单次最多 5 个，避免耗时太长

log "需要补 $NEED 个账号，启动注册容器..."
docker rm -f "$CONTAINER_NAME" 2>/dev/null

# 后台运行，等会儿再查结果
docker run -d --name "$CONTAINER_NAME" --network host \
  --security-opt seccomp=unconfined \
  -v "$APP_DIR:/app" \
  -e http_proxy=http://172.18.45.188:7891 \
  -e https_proxy=http://172.18.45.188:7891 \
  --entrypoint /app/register-entrypoint.sh \
  "$IMAGE" --count "$NEED" --mail-provider yydsmail >> "$LOG" 2>&1

# 等注册完成（最多 15 分钟）
docker wait "$CONTAINER_NAME" 2>/dev/null &
WAITPID=$!
for i in $(seq 180); do  # 180 × 5s = 15min
  if ! kill -0 "$WAITPID" 2>/dev/null; then break; fi
  sleep 5
done
kill "$WAITPID" 2>/dev/null

NEW_TOTAL=$(fetch_total)
log "补号完成，当前总 credits: $NEW_TOTAL"