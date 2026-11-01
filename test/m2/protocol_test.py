#!/usr/bin/env python3
"""PhoneFS M2 写端点真机黑盒（stdlib only）。

PHONEFS_TOKEN=... python -I protocol_test.py
默认打 https://192.168.43.1:18443，可用 PHONEFS_BASE_URL 覆盖。
所有写操作限定 /storage/emulated/0/PhoneFS-m2-protocol，结束递归清理。
"""
import base64
import hashlib
import http.client
import json
import os
import random
import ssl
import time
from urllib.parse import quote, urlsplit

BASE = os.environ.get("PHONEFS_BASE_URL", "https://192.168.43.1:18443")
TOKEN = os.environ["PHONEFS_TOKEN"]
ROOT = "/storage/emulated/0/PhoneFS-m2-protocol"
RESULTS = []


def doc_id(path):
    return "p:" + base64.urlsafe_b64encode(path.encode()).decode().rstrip("=")


def request(method, path, body=None, headers=None, expect=None, code=None):
    u = urlsplit(BASE)
    conn = http.client.HTTPSConnection(u.hostname, u.port or 443, timeout=45,
                                       context=ssl._create_unverified_context())
    h = {"Authorization": "Bearer " + TOKEN}
    if headers:
        h.update(headers)
    if isinstance(body, (dict, list)):
        body = json.dumps(body, ensure_ascii=False).encode()
        h["Content-Type"] = "application/json"
    conn.request(method, path, body=body, headers=h)
    resp = conn.getresponse()
    data = resp.read()
    status = resp.status
    conn.close()
    parsed = None
    if data:
        try:
            parsed = json.loads(data)
        except (json.JSONDecodeError, UnicodeDecodeError):
            parsed = None
    if expect is not None and status != expect:
        raise AssertionError(f"{method} {path}: status={status} want={expect} body={data[:300]!r}")
    if code is not None:
        actual = (parsed or {}).get("error", {}).get("code")
        if actual != code:
            raise AssertionError(f"{method} {path}: error={actual} want={code} body={data[:300]!r}")
    return status, parsed, data


def case(name, fn):
    start = time.time()
    try:
        detail = fn()
        RESULTS.append({"name": name, "pass": True, "detail": detail or "ok"})
        print(f"PASS {name}: {detail or 'ok'}", flush=True)
    except Exception as exc:
        RESULTS.append({"name": name, "pass": False, "detail": f"{type(exc).__name__}: {exc}"})
        print(f"FAIL {name}: {type(exc).__name__}: {exc}", flush=True)


def create(parent, name, is_dir=False, expect=201):
    return request("POST", "/v1/fs/create", {"parent": parent, "name": name, "is_dir": is_dir},
                   expect=expect)[1]


def put_full(path, data, mtime=None, expect=200):
    headers = {}
    if mtime is not None:
        headers["X-Set-Mtime"] = str(mtime)
    return request("PUT", "/v1/fs/content?id=" + quote(doc_id(path)), data, headers, expect=expect)


def put_chunk(path, upload_id, start, chunk, total, expect, code=None, mtime=None):
    headers = {"Content-Range": f"bytes {start}-{start + len(chunk) - 1}/{total}",
               "X-Upload-Id": upload_id}
    if mtime is not None:
        headers["X-Set-Mtime"] = str(mtime)
    return request("PUT", "/v1/fs/content?id=" + quote(doc_id(path)), chunk, headers,
                   expect=expect, code=code)


def get_content(path, expect=200):
    return request("GET", "/v1/fs/content?id=" + quote(doc_id(path)), expect=expect)


def main():
    root_id = doc_id(ROOT)
    request("DELETE", "/v1/fs?id=" + quote(root_id) + "&recursive=true", expect=204)
    case("create-root-201", lambda: create("stor:primary", ROOT.rsplit("/", 1)[-1], True)["name"])

    def duplicate():
        create(root_id, "dup", True)
        request("POST", "/v1/fs/create", {"parent": root_id, "name": "dup", "is_dir": True},
                expect=409, code="ALREADY_EXISTS")
        return "ALREADY_EXISTS"
    case("create-duplicate-409", duplicate)
    case("create-invalid-name-400", lambda: request("POST", "/v1/fs/create",
         {"parent": root_id, "name": "bad/name", "is_dir": False}, expect=400, code="INVALID_NAME")[0])
    case("create-missing-parent-404", lambda: request("POST", "/v1/fs/create",
         {"parent": doc_id(ROOT + "/missing"), "name": "x", "is_dir": False}, expect=404, code="NOT_FOUND")[0])

    rng = random.Random(20261101)
    full_data = rng.randbytes(1024 * 1024 + 123)
    replacement = rng.randbytes(123457)
    full_path = ROOT + "/full.bin"
    case("create-full-file-201", lambda: create(root_id, "full.bin")["id"])
    case("full-write-sha-and-mtime", lambda: full_write_and_verify(full_path, full_data, 1700000000123, root_id))
    case("full-atomic-replace-shorter", lambda: full_write_and_verify(full_path, replacement, 1700000000456, root_id))

    empty_path = ROOT + "/empty.bin"
    def empty_write():
        create(root_id, "empty.bin")
        put_full(empty_path, b"")
        _, _, body = get_content(empty_path)
        assert body == b""
        return "zero-length PUT/GET sha=" + hashlib.sha256(body).hexdigest()
    case("full-empty-write", empty_write)

    chunks = [rng.randbytes(65536), rng.randbytes(70000), rng.randbytes(33333)]
    chunk_data = b"".join(chunks)
    chunk_path = ROOT + "/chunk.bin"
    upload_id = "m2-out-of-order-20261101"
    def chunked():
        create(root_id, "chunk.bin")
        put_chunk(chunk_path, upload_id, len(chunks[0]) + len(chunks[1]), chunks[2], len(chunk_data),
                  409, "UPLOAD_INCOMPLETE", mtime=1700000000789)
        off = 0
        r1 = put_chunk(chunk_path, upload_id, off, chunks[0], len(chunk_data), 200)[1]
        assert r1["complete"] is False
        # 同 id+offset 未完成重传覆盖同一块，仍不完成。
        r1b = put_chunk(chunk_path, upload_id, off, chunks[0], len(chunk_data), 200)[1]
        assert r1b["complete"] is False
        off += len(chunks[0])
        # 最后到的是逻辑中块：区间覆盖已全，必须完成而不是死等绝对末块。
        r2 = put_chunk(chunk_path, upload_id, off, chunks[1], len(chunk_data), 200,
                       mtime=1700000000789)[1]
        assert r2["complete"] is True and r2["received"] == len(chunk_data), r2
        _, _, body = get_content(chunk_path)
        assert hashlib.sha256(body).digest() == hashlib.sha256(chunk_data).digest()
        # 完成后的同会话重传不得重改目标，且仍 complete=true。
        r3 = put_chunk(chunk_path, upload_id, off, chunks[1], len(chunk_data), 200)[1]
        assert r3["complete"] is True
        _, _, body2 = get_content(chunk_path)
        assert body2 == chunk_data
        return f"{len(chunk_data)}B out-of-order complete; sha={hashlib.sha256(body).hexdigest()}; retransmit idempotent"
    case("chunk-out-of-order-and-idempotent", chunked)

    incomplete_path = ROOT + "/incomplete.bin"
    def incomplete():
        create(root_id, "incomplete.bin")
        put_chunk(incomplete_path, "m2-incomplete", 900, b"z" * 100, 1000, 409, "UPLOAD_INCOMPLETE")
        return "last chunk with missing prefix -> 409; session retained"
    case("chunk-missing-409", incomplete)

    quota_path = ROOT + "/quota.bin"
    def quota():
        create(root_id, "quota.bin")
        _, sf, _ = request("GET", "/v1/fs/statfs", expect=200)
        total = sf["free_bytes"] + 1024 ** 3
        put_chunk(quota_path, "m2-quota", 0, b"q" * 100, total, 507, "INSUFFICIENT_STORAGE")
        return f"declared total={total} > free={sf['free_bytes']} -> 507 before allocation"
    case("chunk-quota-507", quota)

    renamed_path = ROOT + "/renamed.bin"
    def rename_file():
        _, attr, _ = request("PATCH", "/v1/fs/meta", {"id": doc_id(full_path), "name": "renamed.bin"}, expect=200)
        assert attr["name"] == "renamed.bin"
        get_content(full_path, 404)
        _, _, body = get_content(renamed_path)
        assert body == replacement
        return "old id 404; new name readback sha=" + hashlib.sha256(body).hexdigest()
    case("meta-file-rename", rename_file)

    moved_dir = ROOT + "/moved"
    moved_path = moved_dir + "/renamed.bin"
    def move_file():
        create(root_id, "moved", True)
        _, attr, _ = request("PATCH", "/v1/fs/meta",
                            {"id": doc_id(renamed_path), "parent_id": doc_id(moved_dir)}, expect=200)
        assert attr["parent"] == doc_id(moved_dir)
        _, _, body = get_content(moved_path)
        assert body == replacement
        return "file moved across directories; bytes unchanged"
    case("meta-file-move", move_file)
    case("meta-directory-501", lambda: request("PATCH", "/v1/fs/meta",
         {"id": doc_id(moved_dir), "name": "moved2"}, expect=501, code="NOT_SUPPORTED")[0])

    def meta_conflict():
        create(root_id, "conflict-a")
        create(root_id, "conflict-b")
        request("PATCH", "/v1/fs/meta", {"id": doc_id(ROOT + "/conflict-a"), "name": "conflict-b"},
                expect=409, code="ALREADY_EXISTS")
        return "rename onto existing file -> 409"
    case("meta-conflict-409", meta_conflict)
    case("meta-mtime", lambda: meta_mtime(moved_path))

    case("delete-not-empty-409", lambda: request("DELETE", "/v1/fs?id=" + quote(root_id),
         expect=409, code="NOT_EMPTY")[0])
    case("delete-missing-idempotent-204", lambda: request("DELETE",
         "/v1/fs?id=" + quote(doc_id(ROOT + "/not-there")), expect=204)[0])
    case("delete-recursive-cleanup-204", lambda: request("DELETE",
         "/v1/fs?id=" + quote(root_id) + "&recursive=true", expect=204)[0])
    case("deleted-root-get-404", lambda: get_content(ROOT, 404)[0])

    passed = sum(r["pass"] for r in RESULTS)
    summary = {"base_url": BASE, "isolated_root": ROOT, "pass": passed,
               "fail": len(RESULTS) - passed, "results": RESULTS}
    out = os.path.join(os.path.dirname(__file__), "protocol-results.json")
    with open(out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=2)
    print(f"RESULT pass={passed} fail={len(RESULTS) - passed} evidence={out}", flush=True)
    if passed != len(RESULTS):
        raise SystemExit(1)


def full_write_and_verify(path, data, mtime, parent_id):
    resp = put_full(path, data, mtime)[1]
    assert resp["received"] == len(data) and resp["complete"] is True
    _, _, body = get_content(path)
    assert body == data
    _, listing, _ = request("GET", "/v1/fs/list?id=" + quote(parent_id), expect=200)
    entry = next(e for e in listing["entries"] if e["id"] == doc_id(path))
    # TEQU-S2C /storage/emulated/0 的 setLastModified 实测为秒级精度，不能按毫秒等值断言。
    assert entry["modified_ms"] == (mtime // 1000) * 1000, entry
    return f"{len(data)}B sha={hashlib.sha256(body).hexdigest()} mtime={entry['modified_ms']} (fs seconds)"


def meta_mtime(path):
    _, attr, _ = request("PATCH", "/v1/fs/meta", {"id": doc_id(path), "modified_ms": 1700000000999}, expect=200)
    assert attr["modified_ms"] == 1700000000000, attr
    return "modified_ms=1700000000000 (requested 1700000000999, fs seconds)"


if __name__ == "__main__":
    main()
