#!/usr/bin/env bash
# 息屏存活探针：每 30s 一次鉴权请求（statfs + pair/info），默认 5 分钟（11 次）。
# 用法： HOST=192.168.x.x TOKEN=xxx [SECONDS=300] ./screenoff_probe.sh probe.log
set -u
HOST="${HOST:?}"; PORT="${PORT:-18443}"; TOKEN="${TOKEN:?}"
OUT="${1:-probe.log}"; SECONDS_T="${SECONDS:-300}"; INTERVAL=30
END=$(( $(date +%s) + SECONDS_T ))
i=0
echo "start $(date -Is) host=$HOST seconds=$SECONDS_T" | tee "$OUT"
while [ "$(date +%s)" -lt "$END" ]; do
  i=$((i+1))
  CODE=$(curl.exe -sS -k --tlsv1.3 -o /dev/null -w '%{http_code} %{time_total}s' \
      -H "Authorization: Bearer $TOKEN" "https://$HOST:$PORT/v1/fs/statfs" 2>&1)
  echo "$(date '+%H:%M:%S') probe#$i statfs -> $CODE" | tee -a "$OUT"
  sleep "$INTERVAL"
done
echo "end $(date -Is) probes=$i" | tee -a "$OUT"
grep -c ' 200 ' "$OUT" | xargs echo "200-count:"
