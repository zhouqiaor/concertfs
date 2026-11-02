#!/usr/bin/env bash
# PhoneFS Phase 0 codegen 入口（bash / Git Bash）。
#
# 从 contract/openapi/phonefs.yaml 生成：
#   - Go models  -> go/internal/gen/phonefs.gen.go            (oapi-codegen 2.8.2)
#   - Kotlin     -> android/core-protocol/src/generated/kotlin (openapi-generator 7.26.0)
#   - 错误码常量 -> go/internal/fsprovider/error_codes.gen.go
#                   android/.../protocol/ErrCodes.gen.kt      (gen_error_codes.py)
#
# 契约本体只读：生成前由 normalize_codegen_spec.py 在 tools/ 下写一份临时副本，
# 去掉 properties 同层的 required-only anyOf 与显式 additionalProperties: true
# （详见该脚本头注释）。
# 可重复运行：生成目录每次重建，生成物内容确定性，重复运行零 diff。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_DIR"

CONTRACT="contract/openapi/phonefs.yaml"
GO_OUT="go/internal/gen/phonefs.gen.go"
KT_OUT="android/core-protocol/src/generated/kotlin"
TOOLS_DIR="scripts/tools"
NORMALIZED_SPEC="$TOOLS_DIR/.codegen/phonefs.normalized.yaml"

OAPI_CODEGEN_VERSION="v2.8.2"
OPENAPI_GENERATOR_VERSION="7.26.0"
OPENAPI_GENERATOR_SHA256="1760050094997b9cc790cc1350be095f15da8c08a996b184d3eadc4ccda5e35e"
OPENAPI_GENERATOR_JAR="$TOOLS_DIR/openapi-generator-cli-$OPENAPI_GENERATOR_VERSION.jar"
OPENAPI_GENERATOR_URL="https://repo.huaweicloud.com/repository/maven/org/openapitools/openapi-generator-cli/$OPENAPI_GENERATOR_VERSION/openapi-generator-cli-$OPENAPI_GENERATOR_VERSION.jar"

# ---------- 工具探测 ----------
GO_BIN="${GO_BIN:-}"
if [[ -z "$GO_BIN" ]]; then
  # 本机构建机 PATH 上的 go 可能是 386；优先显式 amd64 安装。
  if [[ -x "/c/Programs/go-amd64/bin/go.exe" ]]; then
    GO_BIN="/c/Programs/go-amd64/bin/go.exe"
  else
    GO_BIN="go"
  fi
fi

PYTHON_BIN="${PYTHON_BIN:-}"
if [[ -z "$PYTHON_BIN" ]]; then
  if command -v python >/dev/null 2>&1; then PYTHON_BIN="python"; else PYTHON_BIN="python3"; fi
fi

JAVA_BIN="${JAVA_BIN:-}"
if [[ -z "$JAVA_BIN" ]]; then
  if [[ -n "${JAVA_HOME:-}" && -x "$JAVA_HOME/bin/java" ]]; then
    JAVA_BIN="$JAVA_HOME/bin/java"
  elif command -v java >/dev/null 2>&1; then
    JAVA_BIN="java"
  else
    echo "error: java not found (set JAVA_HOME or JAVA_BIN)" >&2
    exit 1
  fi
fi

if command -v cygpath >/dev/null 2>&1; then
  GOPATH_DIR="$(cygpath -u "$("$GO_BIN" env GOPATH)")"
else
  GOPATH_DIR="$("$GO_BIN" env GOPATH)"
fi

OAPI_BIN="${OAPI_CODEGEN:-}"
if [[ -z "$OAPI_BIN" ]]; then
  if command -v oapi-codegen >/dev/null 2>&1; then
    OAPI_BIN="$(command -v oapi-codegen)"
  elif [[ -x "$GOPATH_DIR/bin/oapi-codegen.exe" ]]; then
    OAPI_BIN="$GOPATH_DIR/bin/oapi-codegen.exe"
  elif [[ -x "$GOPATH_DIR/bin/oapi-codegen" ]]; then
    OAPI_BIN="$GOPATH_DIR/bin/oapi-codegen"
  else
    echo "==> installing oapi-codegen $OAPI_CODEGEN_VERSION"
    "$GO_BIN" install "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$OAPI_CODEGEN_VERSION"
    if [[ -x "$GOPATH_DIR/bin/oapi-codegen.exe" ]]; then
      OAPI_BIN="$GOPATH_DIR/bin/oapi-codegen.exe"
    else
      OAPI_BIN="$GOPATH_DIR/bin/oapi-codegen"
    fi
  fi
fi
if [[ ! -x "$OAPI_BIN" ]]; then
  echo "error: oapi-codegen not executable at $OAPI_BIN" >&2
  exit 1
fi

mkdir -p "$TOOLS_DIR"

# ---------- 下载并校验 openapi-generator ----------
if [[ ! -f "$OPENAPI_GENERATOR_JAR" ]]; then
  echo "==> downloading openapi-generator-cli $OPENAPI_GENERATOR_VERSION"
  curl -fSL --retry 3 --connect-timeout 20 -o "$OPENAPI_GENERATOR_JAR.part" "$OPENAPI_GENERATOR_URL"
  echo "$OPENAPI_GENERATOR_SHA256  $OPENAPI_GENERATOR_JAR.part" | sha256sum -c -
  mv "$OPENAPI_GENERATOR_JAR.part" "$OPENAPI_GENERATOR_JAR"
fi

ACTUAL_SHA256="$(sha256sum "$OPENAPI_GENERATOR_JAR" | cut -d' ' -f1)"
if [[ "$ACTUAL_SHA256" != "$OPENAPI_GENERATOR_SHA256" ]]; then
  echo "error: openapi-generator jar sha256 mismatch: $ACTUAL_SHA256" >&2
  rm -f "$OPENAPI_GENERATOR_JAR"
  exit 1
fi
echo "==> openapi-generator jar sha256 OK ($ACTUAL_SHA256)"

# ---------- 1. 规范化临时副本 ----------
mkdir -p "$(dirname "$NORMALIZED_SPEC")"
"$PYTHON_BIN" scripts/normalize_codegen_spec.py "$CONTRACT" "$NORMALIZED_SPEC"

# ---------- 2. Go models ----------
mkdir -p "$(dirname "$GO_OUT")"
echo "==> go: $(basename "$OAPI_BIN") -generate types"
"$OAPI_BIN" -generate types -package gen -o "$GO_OUT" "$NORMALIZED_SPEC"

# ---------- 3. Kotlin models ----------
rm -rf "$KT_OUT"
echo "==> kotlin: openapi-generator $OPENAPI_GENERATOR_VERSION (models only)"
"$JAVA_BIN" -jar "$OPENAPI_GENERATOR_JAR" generate \
  -i "$NORMALIZED_SPEC" \
  -g kotlin \
  --type-mappings="object=JsonObject" \
  --import-mappings="JsonObject=kotlinx.serialization.json.JsonObject" \
  --additional-properties="serializationLibrary=kotlinx_serialization,enumPropertyNaming=UPPERCASE,modelPackage=com.phonefs.core.protocol.gen,sourceFolder=,openApiNullable=false" \
  --global-property="models,modelDocs=false,modelTests=false,supportingFiles=false" \
  -o "$KT_OUT" >"$TOOLS_DIR/kotlin-generator.log" 2>&1 || {
    cat "$TOOLS_DIR/kotlin-generator.log" >&2
    exit 1
  }
# 生成器元数据与空脚手架目录不随源码提交
rm -rf "$KT_OUT/.openapi-generator"
find "$KT_OUT" -mindepth 1 -type d -empty -delete

# ---------- 4. 错误码常量 ----------
"$PYTHON_BIN" scripts/gen_error_codes.py

# ---------- 汇总 ----------
echo "==> generated:"
echo "    $GO_OUT ($(wc -l < "$GO_OUT") lines)"
echo "    $KT_OUT ($(find "$KT_OUT" -name '*.kt' | wc -l | tr -d ' ') files)"
echo "    go/internal/fsprovider/error_codes.gen.go"
echo "    android/core-protocol/src/main/kotlin/com/phonefs/core/protocol/ErrCodes.gen.kt"

rm -f "$NORMALIZED_SPEC"
rmdir "$(dirname "$NORMALIZED_SPEC")" 2>/dev/null || true
