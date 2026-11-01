#!/usr/bin/env python3
"""PhoneFS M3 delta/rename 真机黑盒（stdlib only）。

PHONEFS_TOKEN=... python -I protocol_test.py
默认打 https://192.168.43.1:18443，可用 PHONEFS_BASE_URL 覆盖。
所有写操作限定 /storage/emulated/0/PhoneFS-m3-protocol，结束递归清理。
"""
import base64
import hashlib
import http.client
import json
import os
import ssl
import time
from urllib.parse import quote, urlsplit

BASE = os.environ.get("PHONEFS_BASE_URL", "https://192.168.43.1:18443")
TOKEN = os.environ["PHONEFS_TOKEN"]
ROOT = "/storage/emulated/0/PhoneFS-m3-protocol"
RUN_ID = str(time.time_ns())
RESULTS = []


class EnvironmentLimited(Exception):
    pass


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
        RESULTS.append({"name": name, "pass": True, "detail": detail or "ok",
                        "duration_ms": round((time.time() - start) * 1000, 1)})
        print(f"PASS {name}: {detail or 'ok'}", flush=True)
    except EnvironmentLimited as exc:
        RESULTS.append({"name": name, "pass": False, "environment_limited": True,
                        "detail": str(exc), "duration_ms": round((time.time() - start) * 1000, 1)})
        print(f"ENV_LIMIT {name}: {exc}", flush=True)
    except Exception as exc:
        RESULTS.append({"name": name, "pass": False,
                        "detail": f"{type(exc).__name__}: {exc}",
                        "duration_ms": round((time.time() - start) * 1000, 1)})
        print(f"FAIL {name}: {type(exc).__name__}: {exc}", flush=True)


def create(parent, name, is_dir=False, expect=201):
    return request("POST", "/v1/fs/create", {"parent": parent, "name": name, "is_dir": is_dir},
                   expect=expect)[1]


def put_full(path, data, mtime=None, expect=200):
    headers = {}
    if mtime is not None:
        headers["X-Set-Mtime"] = str(mtime)
    return request("PUT", "/v1/fs/content?id=" + quote(doc_id(path)), data, headers, expect=expect)


def put_chunk(path, upload_id, start, chunk, total, expect=200, code=None,
              base_etag=None, final=False):
    headers = {"Content-Range": f"bytes {start}-{start + len(chunk) - 1}/{total}",
               "X-Upload-Id": upload_id}
    if base_etag is not None:
        headers["X-Base-ETag"] = base_etag
    if final:
        headers["X-Upload-Final"] = "true"
    return request("PUT", "/v1/fs/content?id=" + quote(doc_id(path)), chunk, headers,
                   expect=expect, code=code)


def get_content(path, expect=200):
    return request("GET", "/v1/fs/content?id=" + quote(doc_id(path)), expect=expect)


def patch_meta(path, name, replace=None, expect=200, code=None):
    body = {"id": doc_id(path), "name": name}
    if replace is not None:
        body["replace"] = replace
    return request("PATCH", "/v1/fs/meta", body, expect=expect, code=code)


def list_entries(parent_id):
    _, listing, _ = request("GET", "/v1/fs/list?id=" + quote(parent_id), expect=200)
    return listing["entries"]


def make_file(root_id, name, data):
    path = ROOT + "/" + name
    create(root_id, name)
    _, result, _ = put_full(path, data)
    assert result["complete"] is True and result["received"] == len(data), result
    assert get_content(path)[2] == data
    return path, result["etag"]


def upload_id(label):
    return "m3-" + RUN_ID + "-" + label


def sha(data):
    return hashlib.sha256(data).hexdigest()


def main():
    root_id = doc_id(ROOT)
    request("DELETE", "/v1/fs?id=" + quote(root_id) + "&recursive=true", expect=204)
    case("create-root-201", lambda: create("stor:primary", ROOT.rsplit("/", 1)[-1], True)["name"])
    try:
        def delta_nonfinal_and_final():
            original = bytes((i * 17 + 3) % 256 for i in range(128))
            path, base = make_file(root_id, "delta-ranges.bin", original)
            first = b"ABCDE"
            second = b"xyz123"
            _, pending, _ = put_chunk(path, upload_id("ranges"), 11, first, len(original),
                                      base_etag=base)
            assert pending["complete"] is False, pending
            assert get_content(path)[2] == original, "non-final delta changed target"
            _, done, _ = put_chunk(path, upload_id("ranges"), 61, second, len(original),
                                   base_etag=base, final=True)
            assert done["complete"] is True and done["received"] == len(original), done
            expected = bytearray(original)
            expected[11:11 + len(first)] = first
            expected[61:61 + len(second)] = second
            actual = get_content(path)[2]
            assert actual == bytes(expected), "final changed bytes outside supplied ranges"
            return f"non-final left 128B unchanged; final patched [11,16) and [61,67); sha={sha(actual)}"
        case("delta-nonfinal-no-commit-final-ranges-only", delta_nonfinal_and_final)

        def delta_extension():
            original = b"original-prefix-must-survive-delta-extension"
            extension = b"-and-this-is-the-new-suffix"
            path, base = make_file(root_id, "delta-extend.bin", original)
            total = len(original) + len(extension)
            _, done, _ = put_chunk(path, upload_id("extend"), len(original), extension, total,
                                   base_etag=base, final=True)
            assert done["complete"] is True and done["received"] == total, done
            actual = get_content(path)[2]
            assert actual == original + extension
            assert actual[:len(original)] == original
            return f"{len(original)}B prefix retained; extended to {total}B; sha={sha(actual)}"
        case("delta-extension-preserves-prefix", delta_extension)

        def delta_base_mismatch():
            original = b"base-mismatch-must-not-modify-these-bytes"
            path, _ = make_file(root_id, "delta-wrong-base.bin", original)
            put_chunk(path, upload_id("wrong-base"), 0, b"BAD", len(original),
                      expect=412, code="PRECONDITION_FAILED", base_etag='"0-0"')
            assert get_content(path)[2] == original
            return "stale base -> 412 PRECONDITION_FAILED; original bytes unchanged"
        case("delta-wrong-base-412-preserves-target", delta_base_mismatch)

        def delta_missing_target():
            path = ROOT + "/delta-missing.bin"
            put_chunk(path, upload_id("missing"), 0, b"X", 1,
                      expect=412, code="PRECONDITION_FAILED", base_etag='"1-1"')
            get_content(path, 404)
            return "missing delta target -> 412 PRECONDITION_FAILED; target remains absent"
        case("delta-missing-target-412-remains-absent", delta_missing_target)

        def delta_concurrent_change():
            original = b"original-base-before-concurrent-write"
            external = b"new-concurrent-full-value-with-different-length"
            path, base = make_file(root_id, "delta-concurrent.bin", original)
            _, pending, _ = put_chunk(path, upload_id("concurrent"), 2, b"aa", len(original),
                                      base_etag=base)
            assert pending["complete"] is False
            put_full(path, external)
            assert get_content(path)[2] == external
            put_chunk(path, upload_id("concurrent"), 7, b"bb", len(original),
                      expect=412, code="PRECONDITION_FAILED", base_etag=base, final=True)
            assert get_content(path)[2] == external
            return "whole-file write after session start -> final 412; concurrent replacement preserved"
        case("delta-concurrent-write-final-412-preserves-new", delta_concurrent_change)

        def delta_final_retransmit():
            original = b"abcdefgh"
            path, base = make_file(root_id, "delta-retransmit.bin", original)
            _, first, _ = put_chunk(path, upload_id("retransmit"), 2, b"XY", len(original),
                                    base_etag=base, final=True)
            committed = get_content(path)[2]
            assert committed == b"abXYefgh"
            _, replay, _ = put_chunk(path, upload_id("retransmit"), 2, b"XY", len(original),
                                     base_etag=base, final=True)
            assert replay == first, (first, replay)
            assert get_content(path)[2] == committed
            return f"identical final response on retransmit; bytes stable; sha={sha(committed)}"
        case("delta-final-retransmit-idempotent", delta_final_retransmit)

        def upload_id_mode_mixing():
            original = b"12345678"
            path, base = make_file(root_id, "delta-mode.bin", original)
            _, pending, _ = put_chunk(path, upload_id("legacy-first"), 0, b"a", len(original))
            assert pending["complete"] is False
            put_chunk(path, upload_id("legacy-first"), 1, b"b", len(original),
                      expect=400, code="INVALID_ARGUMENT", base_etag=base)
            _, pending, _ = put_chunk(path, upload_id("delta-first"), 0, b"a", len(original),
                                      base_etag=base)
            assert pending["complete"] is False
            put_chunk(path, upload_id("delta-first"), 1, b"b", len(original),
                      expect=400, code="INVALID_ARGUMENT")
            assert get_content(path)[2] == original
            return "legacy->delta and delta->legacy both 400 INVALID_ARGUMENT; target unchanged"
        case("upload-id-cannot-mix-base-modes-400", upload_id_mode_mixing)

        def replace_false_conflict():
            source_data = b"source-must-survive-conflict"
            target_data = b"target-must-survive-conflict"
            source, _ = make_file(root_id, "replace-false-source.bin", source_data)
            target, _ = make_file(root_id, "replace-false-target.bin", target_data)
            patch_meta(source, target.rsplit("/", 1)[-1], replace=False,
                       expect=409, code="ALREADY_EXISTS")
            assert get_content(source)[2] == source_data
            assert get_content(target)[2] == target_data
            return "replace=false -> 409 ALREADY_EXISTS; source and target unchanged"
        case("patch-replace-false-existing-target-409", replace_false_conflict)

        def replace_true_commit():
            source_data = b"source-content-atomically-replaces-target"
            target_data = b"old-target-content-must-disappear"
            source, _ = make_file(root_id, "replace-true-source.bin", source_data)
            target, _ = make_file(root_id, "replace-true-target.bin", target_data)
            _, attr, _ = patch_meta(source, target.rsplit("/", 1)[-1], replace=True)
            assert attr["name"] == target.rsplit("/", 1)[-1]
            get_content(source, 404)
            assert get_content(target)[2] == source_data
            matches = [e for e in list_entries(root_id)
                       if e["name"] == target.rsplit("/", 1)[-1]]
            assert len(matches) == 1, matches
            return "replace=true -> target has source bytes, source 404, exactly one target entry"
        case("patch-replace-true-existing-target", replace_true_commit)

        def case_only_rename():
            content = b"case-only-rename-content"
            source, _ = make_file(root_id, "CaseOnly.bin", content)
            new_name = "caseonly.bin"
            status, parsed, data = patch_meta(source, new_name, expect=None)
            if status in (409, 501):
                actual_code = (parsed or {}).get("error", {}).get("code")
                if (status, actual_code) not in ((409, "ALREADY_EXISTS"), (501, "NOT_SUPPORTED")):
                    raise AssertionError(f"case-only rename status={status} body={data[:300]!r}")
                assert get_content(source)[2] == content
                names = [e["name"] for e in list_entries(root_id)
                         if e["name"].casefold() == new_name.casefold()]
                assert names == ["CaseOnly.bin"], names
                raise EnvironmentLimited(
                    f"TEQU returned {status} {actual_code}; source name/bytes preserved; "
                    "ROM case-only rename unsupported (not counted as PASS)")
            if status != 200:
                raise AssertionError(f"case-only rename status={status} body={data[:300]!r}")
            assert parsed["name"] == new_name, parsed
            new_path = ROOT + "/" + new_name
            assert get_content(new_path)[2] == content
            names = [e["name"] for e in list_entries(root_id)
                     if e["name"].casefold() == new_name.casefold()]
            if names == ["CaseOnly.bin"]:
                assert get_content(source)[2] == content
                raise EnvironmentLimited(
                    "PATCH returned 200 and new-case lookup resolves, but directory listing "
                    "retains CaseOnly.bin; TEQU ROM did not materialize case-only rename "
                    "(not counted as PASS)")
            assert names == [new_name], names
            old_status, _, old_data = get_content(source, expect=None)
            assert old_status in (200, 404), old_status
            if old_status == 200:
                assert old_data == content
            return (f"rename succeeded; listing has only {new_name}; "
                    f"old-case lookup={old_status} (ROM path semantics)")
        case("patch-case-only-rename-tequ", case_only_rename)
    finally:
        case("cleanup-recursive-204", lambda: request(
            "DELETE", "/v1/fs?id=" + quote(root_id) + "&recursive=true", expect=204)[0])
        case("cleanup-root-get-404", lambda: get_content(ROOT, 404)[0])

    passed = sum(r["pass"] for r in RESULTS)
    limited = sum(bool(r.get("environment_limited")) for r in RESULTS)
    failed = len(RESULTS) - passed - limited
    summary = {"base_url": BASE, "isolated_root": ROOT, "run_id": RUN_ID,
               "pass": passed, "fail": failed, "environment_limited": limited,
               "results": RESULTS}
    out = os.path.join(os.path.dirname(__file__), "protocol-results.json")
    with open(out, "w", encoding="utf-8") as f:
        json.dump(summary, f, ensure_ascii=False, indent=2)
    print(f"RESULT pass={passed} fail={failed} environment_limited={limited} evidence={out}",
          flush=True)
    if failed:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
