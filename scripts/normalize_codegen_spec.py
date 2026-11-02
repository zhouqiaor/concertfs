#!/usr/bin/env python3
"""生成前规范化：为 codegen 写出契约的临时副本（契约本体不改）。

背景（Phase 0 实测坑）：
  `MetaReq` 这类 schema 把 `properties` 与同层 `anyOf: [{required: [x]}, ...]`
  混用。该 anyOf 只表达「至少出现一个字段」的校验约束，不改变 wire 类型。
  - openapi-generator 7.26.0（kotlin）：整张 schema 的 properties 被丢弃，
    生成空 data class；
  - oapi-codegen 2.8.2：生成 `interface{}` union + `json.RawMessage` 包装与
    `github.com/oapi-codegen/runtime` 依赖。

本脚本只在**内存副本**中删除这类「分支仅含 required/description/title」的
anyOf/oneOf，使两端都按 properties 生成普通 DTO。契约文件
（contract/openapi/phonefs.yaml）本身不做任何修改。

用法：
  python normalize_codegen_spec.py <input.yaml> <output.yaml>
"""

from __future__ import annotations

import sys

import yaml

_ALLOWED_BRANCH_KEYS = {"required", "description", "title"}


def _is_required_only_branch(branch: object) -> bool:
    if not isinstance(branch, dict):
        return False
    if not set(branch).issubset(_ALLOWED_BRANCH_KEYS):
        return False
    required = branch.get("required")
    return isinstance(required, list) and all(isinstance(k, str) for k in required)


def strip_required_only_composition(node: object, path: str, removed: list[str]) -> None:
    """递归删除 properties 同层的 required-only anyOf/oneOf。

    同时删除显式 `additionalProperties: true`：JSON Schema 中它是默认值，
    语义等价；但 openapi-generator 会把它写成 `Map<String, Any>`，kotlinx
    序列化插件在 IR 阶段要求 Any 的序列化器，导致 `:core-protocol` 编译失败。
    删除后该字段按自由对象处理，配合 `object=JsonObject` 类型映射生成
    可序列化的 `kotlinx.serialization.json.JsonObject`。
    """
    if isinstance(node, dict):
        if node.get("additionalProperties") is True:
            del node["additionalProperties"]
            removed.append(f"{path}.additionalProperties(=true)")
        if "properties" in node:
            for keyword in ("anyOf", "oneOf"):
                branches = node.get(keyword)
                if isinstance(branches, list) and branches and all(
                    _is_required_only_branch(b) for b in branches
                ):
                    del node[keyword]
                    removed.append(f"{path}.{keyword}")
        for key, value in node.items():
            strip_required_only_composition(value, f"{path}.{key}", removed)
    elif isinstance(node, list):
        for index, value in enumerate(node):
            strip_required_only_composition(value, f"{path}[{index}]", removed)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__.strip().splitlines()[-1], file=sys.stderr)
        return 2

    source, target = argv[1], argv[2]
    with open(source, encoding="utf-8") as handle:
        document = yaml.safe_load(handle)

    removed: list[str] = []
    strip_required_only_composition(document, "$", removed)

    with open(target, "w", encoding="utf-8", newline="\n") as handle:
        yaml.safe_dump(
            document,
            handle,
            allow_unicode=True,
            sort_keys=False,
            width=4096,
        )

    for path in removed:
        print(f"normalize_codegen_spec: dropped required-only composition at {path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
