#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
PhoneFS M0-W2 吞吐闸门：10MB 分块 × N 次 Range 拉流（默认 1000 次 = 10GB）。

- 仅标准库；强制 TLS 1.3；默认做证书指纹固定（--pin <hex64>，与 /v1/pair/info fingerprint 比对），
  --insecure 时跳过校验（两种模式在报告中记录，W2 正式数据走 pin）。
- 单条 HTTP/1.1 keep-alive 连接顺序拉流（单连接顺序读场景，对齐 SPEC §6.7 连接池模型的一槽）。
- 输出人读表 + JSON（总量/总耗时/平均 MiB/s/p50/p95/失败/重连/TLS 版本套件）。

用法：
  python throughput_test.py --host 192.168.x.x --port 18443 \
      --token TOKEN --pin HEX64 --id 'p:BASE64URL' [--count 1000] [--chunk 10485760] \
      [--out w2-throughput.json]
"""
import argparse
import http.client
import json
import socket
import ssl
import statistics
import sys
import time
from urllib.parse import quote

BUF = 256 * 1024


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", required=True)
    ap.add_argument("--port", type=int, default=18443)
    ap.add_argument("--token", required=True)
    ap.add_argument("--id", dest="doc_id", required=True, help="docId，如 p:base64url 或 stor:primary")
    ap.add_argument("--pin", default=None, help="证书 SHA-256 指纹 hex64（TLS pin）")
    ap.add_argument("--insecure", action="store_true", help="不校验指纹（-k 等价，必须在报告记录）")
    ap.add_argument("--count", type=int, default=1000)
    ap.add_argument("--chunk", type=int, default=10 * 1024 * 1024)
    ap.add_argument("--out", default=None)
    args = ap.parse_args()

    if not args.insecure and not args.pin:
        print("ERROR: 必须给 --pin <fingerprint> 或显式 --insecure", file=sys.stderr)
        return 2

    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE  # 自签：信任锚=指纹
    ctx.minimum_version = ssl.TLSVersion.TLSv1_3
    ctx.maximum_version = ssl.TLSVersion.TLSv1_3

    path = "/v1/fs/content?id=" + quote(args.doc_id, safe=":")
    rng = f"bytes=0-{args.chunk - 1}"
    headers = {"Authorization": f"Bearer {args.token}", "Range": rng}

    conn = http.client.HTTPSConnection(args.host, args.port, context=ctx, timeout=60)
    tls_info = {}
    failures = []
    lat_ms = []
    bytes_total = 0
    reconnects = 0
    verified = False

    def connect() -> None:
        nonlocal conn, tls_info, verified, reconnects
        if reconnects:
            conn.close()
            conn = http.client.HTTPSConnection(args.host, args.port, context=ctx, timeout=60)
        conn.connect()
        reconnects += 1
        der = conn.sock.getpeercert(binary_form=True)
        import hashlib
        fp = hashlib.sha256(der).hexdigest()
        tls_info = {"version": conn.sock.version(), "cipher": conn.sock.cipher()[0], "fingerprint": fp}
        if not args.insecure:
            if fp.lower() != args.pin.lower():
                raise SystemExit(f"FINGERPRINT MISMATCH server={fp} pinned={args.pin}")
        verified = True
        print(f"TLS {tls_info['version']} {tls_info['cipher']} fp={fp[:16]}… pinned={not args.insecure}")

    t0 = time.perf_counter()
    connect()
    for i in range(1, args.count + 1):
        try:
            t1 = time.perf_counter()
            conn.request("GET", path, headers=headers)
            resp = conn.getresponse()
            got = 0
            while True:
                chunk = resp.read(BUF)
                if not chunk:
                    break
                got += len(chunk)
            dt = (time.perf_counter() - t1) * 1000.0
            if resp.status != 206 or got != args.chunk:
                failures.append({"i": i, "status": resp.status, "bytes": got})
            else:
                lat_ms.append(dt)
                bytes_total += got
            if i % 100 == 0:
                el = time.perf_counter() - t0
                print(f"  {i}/{args.count} ok={len(lat_ms)} fail={len(failures)} "
                      f"{bytes_total / 1048576:.0f} MiB / {el:.1f}s = {bytes_total/1048576/el:.2f} MiB/s")
        except (http.client.RemoteDisconnected, ConnectionError, socket.timeout, OSError) as e:
            failures.append({"i": i, "error": type(e).__name__ + ":" + str(e)[:80]})
            try:
                connect()
            except Exception as e2:
                print(f"reconnect failed: {e2}", file=sys.stderr)
                time.sleep(1.0)
                try:
                    connect()
                except Exception:
                    pass
    wall = time.perf_counter() - t0
    conn.close()

    def pct(p: float) -> float:
        if not lat_ms:
            return float("nan")
        return statistics.quantiles(lat_ms, n=100, method="inclusive")[p - 1]

    result = {
        "host": args.host, "port": args.port, "mode": "pin" if not args.insecure else "insecure",
        "tls": tls_info,
        "chunk_bytes": args.chunk, "count": args.count,
        "ok": len(lat_ms), "failures": len(failures), "reconnects": reconnects - 1,
        "total_bytes": bytes_total,
        "wall_seconds": round(wall, 3),
        "avg_MiB_s": round(bytes_total / 1048576 / wall, 3),
        "p50_chunk_ms": round(statistics.median(lat_ms), 2) if lat_ms else None,
        "p95_chunk_ms": round(pct(95), 2) if lat_ms else None,
        "min_chunk_ms": round(min(lat_ms), 2) if lat_ms else None,
        "max_chunk_ms": round(max(lat_ms), 2) if lat_ms else None,
        "fingerprint_verified": verified,
        "fail_samples": failures[:5],
    }
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(result, f, ensure_ascii=False, indent=2)
    # 任何一次失败不返非零（证据照实记录）；指纹不匹配在前面已 SystemExit
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
