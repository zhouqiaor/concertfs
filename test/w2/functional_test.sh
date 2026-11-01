#!/usr/bin/env bash
# PhoneFS M0-W2 功能矩阵（真实协议行为；-k 模式，功能联调可走 adb forward）。
# 用法： HOST=127.0.0.1 PORT=18443 TOKEN=xxx PIN=hex64 ./functional_test.sh
set -u
HOST="${HOST:-127.0.0.1}"
PORT="${PORT:-18443}"
TOKEN="${TOKEN:?need TOKEN}"
PIN="${PIN:-}"
BASE="https://$HOST:$PORT"
# Windows + Git Bash：curl.exe/python 是原生程序，MSYS 的 /tmp 绝对路径它们不可见。
# 临时目录放当前目录下并用相对路径，保证 bash/curl.exe/python 三方都能读写。
TMP="./.w2tmp-$$"
mkdir -p "$TMP"
trap 'rm -rf "$TMP"' EXIT
PASS=0; FAIL=0

CURL=(curl.exe -sS -k --tlsv1.3 --tls-max 1.3)

ok(){   PASS=$((PASS+1)); printf 'PASS %-28s %s\n' "$1" "${2:-}"; }
bad(){  FAIL=$((FAIL+1)); printf 'FAIL %-28s %s\n' "$1" "${2:-}"; }
check(){ if [ "$1" = "$2" ]; then ok "$3" "$1"; else bad "$3" "want=$2 got=$1 $(cat "$TMP/body" | head -c 200)"; fi; }

enc(){ python -c "import base64,os,sys;print('p:'+base64.urlsafe_b64encode(sys.argv[1].encode('utf-8')).decode().rstrip('='))" "$1"; }
jval(){ python -c "import json,sys;d=json.load(open(sys.argv[1],encoding='utf-8'));
cur=d
for k in sys.argv[2].split('.'): cur=cur.get(k) if isinstance(cur,dict) else cur[int(k)]
print('' if cur is None else cur)" "$TMP/body" "$1"; }
jlen(){ python -c "import json,sys;d=json.load(open(sys.argv[1],encoding='utf-8'));
cur=d
for k in sys.argv[2].split('.'): cur=cur.get(k) if isinstance(cur,dict) else cur[int(k)]
print(len(cur))" "$TMP/body" "$1"; }
has(){ grep -q "$1" "$TMP/body" && ok "$2" || bad "$2" "missing $1"; }
hasre(){ grep -Eq "$1" "$TMP/body" && ok "$2" || bad "$2" "missing /$1/"; }

echo "== 0. TLS1.3 握手证据（openssl s_client） =="
echo | openssl s_client -connect "$HOST:$PORT" -tls1_3 -showcerts -servername phonefs 2>/dev/null > "$TMP/sclient"
grep -Eq "Protocol[[:space:]]*: TLSv1.3" "$TMP/sclient" && ok "tls protocol TLSv1.3" || bad "tls protocol" "$(grep Protocol "$TMP/sclient")"
SUITE=$(grep -oE 'TLS_AES_[A-Z0-9_]+' "$TMP/sclient" | head -1)
echo "    cipher=$SUITE"
# 从 s_client 输出中切 PEM 证书段→DER→SHA256（s_client 文本不能直接喂 x509 -in）
SVR_FP=$(sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' "$TMP/sclient" \
  | openssl x509 -outform DER 2>/dev/null | sha256sum | cut -d' ' -f1)
echo "    server fingerprint=$SVR_FP"
[ -n "$PIN" ] && { [ "$SVR_FP" = "$(echo "$PIN"|tr 'A-F' 'a-f')" ] && ok "fingerprint pin" || bad "fingerprint pin" "server=$SVR_FP pin=$PIN"; }

echo "== 1. GET /v1/pair/info（免鉴权） =="
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "$BASE/v1/pair/info"
check "$(grep -oE 'HTTP/[0-9.]+ [0-9]+' "$TMP/h" | tail -1 | awk '{print $2}')" 200 pair-info-200
check "$(jval service)" phonefs pair-info-service
check "$(jval protocol_versions.0)" 1 pair-info-proto
hasre '"pairing_open": *false' pair-info-pairing-closed
[ ${#SVR_FP} -eq 64 ] && [ "$(jval fingerprint)" = "$SVR_FP" ] && ok pair-info-fingerprint || bad pair-info-fingerprint
jval device.name >/dev/null && ok pair-info-device-name || bad pair-info-device-name

echo "== 2. 401 路径 =="
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "$BASE/v1/roots"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 401 roots-noauth-401
check "$(jval error.code)" TOKEN_INVALID roots-noauth-code
${CURL[@]} -D "$TMP/h" -o "$TMP/body" -H "Authorization: Bearer wrong-token" "$BASE/v1/roots"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 401 roots-badtoken-401

AUTH=(-H "Authorization: Bearer $TOKEN")

echo "== 3. GET /v1/roots =="
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/roots"
check "$(jlen roots)" 8 roots-count-8
has 'stor:primary' roots-stor
if grep -q 'cat:camera' "$TMP/body"; then bad roots-cat-camera-present; else ok "cat:camera 不在固定 8 行"; fi
has '内部存储' roots-stor-name
${CURL[@]} -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/fs/list"
check "$(jlen entries)" 8 list-root-entries-8
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/fs/list?cursor=bogus"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 400 list-root-cursor-400
check "$(jval error.code)" BAD_CURSOR list-root-cursor-code

echo "== 4. list 真实树（含中文/emoji/空格） =="
${CURL[@]} -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/fs/list?id=stor:primary"
has 'PhoneFS-w2' list-stor-root-fixture-dir
DIR_ID=$(enc "/storage/emulated/0/PhoneFS-w2")
${CURL[@]} -o "$TMP/body" "${AUTH[@]}" --get --data-urlencode "id=$DIR_ID" "$BASE/v1/fs/list"
has '测试 文件.txt' list-unicode-space-file
SUB_ID=$(enc "/storage/emulated/0/PhoneFS-w2/中文目录")
${CURL[@]} -o "$TMP/body" "${AUTH[@]}" --get --data-urlencode "id=$SUB_ID" "$BASE/v1/fs/list"
has '截图 😀.bin' list-emoji-file
has 'a b c.md' list-space-md
jval entries.0.media_type >/dev/null 2>&1; [ "$(python -c "import json;d=json.load(open('$TMP/body',encoding='utf-8'));print('media_type' in d['entries'][0])")" = "False" ] \
  && ok "real-tree media_type 缺省" || bad "real-tree media_type 不应出现"

echo "== 5. 路径穿越 =="
EVIL=$(enc "/storage/emulated/0/../../../etc")
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" --get --data-urlencode "id=$EVIL" "$BASE/v1/fs/list"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 400 traversal-400

echo "== 6. content 全量 200 =="
F_ID=$(enc "/storage/emulated/0/PhoneFS-w2/测试 文件.txt")
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" --get --data-urlencode "id=$F_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 200 content-200
grep -qi 'accept-ranges: bytes' "$TMP/h" && ok accept-ranges || bad accept-ranges
ETAG=$(grep -i '^etag:' "$TMP/h" | tr -d '\r' | awk '{print $2}')
echo "    etag=$ETAG"
[ -n "$ETAG" ] && ok etag-present || bad etag
grep -qi 'last-modified:' "$TMP/h" && ok last-modified || bad last-modified

echo "== 7. Range 206 / 416 / If-Range =="
BIG_ID=$(enc "/storage/emulated/0/PhoneFS-w2/test10mb.bin")
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" -H "Range: bytes=0-999" --get --data-urlencode "id=$BIG_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 206 range-206
CR=$(grep -i '^content-range:' "$TMP/h" | tr -d '\r' | sed 's/.*: //')
[ "$CR" = "bytes 0-999/10485760" ] && ok "content-range" "$CR" || bad "content-range" "$CR"
[ "$(wc -c < "$TMP/body")" -eq 1000 ] && ok range-body-1000B || bad range-body-bytes "$(wc -c < "$TMP/body")"
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" -H "Range: bytes=10485760-10485761" --get --data-urlencode "id=$BIG_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 416 range-416
grep -qi 'content-range: bytes \*/10485760' "$TMP/h" && ok "416 content-range" || bad "416 content-range" "$(grep -i content-range "$TMP/h")"
check "$(jval error.code)" RANGE_NOT_SATISFIABLE range-416-code
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" -H "Range: bytes=0-99" -H "If-Range: \"deadbeef\"" --get --data-urlencode "id=$BIG_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 200 ifrange-mismatch-200
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" -H "Range: bytes=0-99" -H "If-Range: $ETAG" --get --data-urlencode "id=$F_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 206 ifrange-match-206
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" -H "Range: bytes=-100" --get --data-urlencode "id=$BIG_ID" "$BASE/v1/fs/content"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 206 suffix-206
[ "$(wc -c < "$TMP/body")" -eq 100 ] && ok suffix-100B || bad suffix-bytes

echo "== 8. statfs / cat 501 / 404 =="
${CURL[@]} -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/fs/statfs"
python -c "import json;d=json.load(open('$TMP/body',encoding='utf-8'));assert d['total_bytes']>0 and d['block_size']>0" && ok statfs-fields || bad statfs-fields
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/fs/list?id=cat:image"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 501 cat-501
${CURL[@]} -D "$TMP/h" -o "$TMP/body" "${AUTH[@]}" "$BASE/v1/nope"
check "$(grep -oE '[0-9]{3}' "$TMP/h" | head -1)" 404 unknown-404

echo
echo "RESULT pass=$PASS fail=$FAIL  tmp=$TMP"
[ "$FAIL" -eq 0 ]
