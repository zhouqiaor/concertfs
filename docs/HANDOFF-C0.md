# PhoneFS C0 阶段交接文档（契约 + 模块拆分）

日期：2026-11-02
分支：`feat-phonefs-m3`（worktree `C:/Code/Wideawake-wt-phonefs-m3`）
状态：**C0 已完成，验证全绿，但尚未 commit**。下一步见 §6。

> 本文件供其他 Claude/Codex/Cursor 会话接手。所有结论均可被下面的命令独立复现，不要只信本文件。

---

## 1. 这一阶段的任务是什么（C0）

Phase 0 收口 OpenAPI 契约 + 双端 codegen 零 diff；并把 Android `:core` 拆成纯协议层 `:core-protocol`（consumser APK 不能继承 Ktor server / Conscrypt / MES 依赖）。

C0 之前的状态（来自 `docs/design/2026-11-02-phonefs-consumer-capability-roadmap.md` §2 现状事实）：
- Android 只有 `:core` + `:source`，无 `:consumer`、无客户端 HTTP 栈、无 DocumentsProvider。
- 服务端只挂了 9 条路由；消费端 P0 依赖的 `pair/hello|commit`、`session/*`、`fs/stat`、`index/meta|snapshot`、`events` 均缺失。
- `:core` 用 `api(...)` 会把整个服务端依赖传递进任何消费它的 APK —— 这是 C0 要解决的核心问题。

## 2. 已完成的工作（落盘未 commit）

### 2.1 契约（Phase 0，`projects/phonefs/contract/`）
- `openapi/phonefs.yaml`：OpenAPI 3.1.0 单文件。30 path / **33 operation**（基础 22 = P0 19 + P1 3(copy/thumb/changes) + Fleet(ADR-18) 3 + Share(ADR-17) 8）。45 错误码（含 `PRECONDITION_FAILED` 412；`ANOTHER_SESSION` deprecated）。**9 个 `x-phonefs-implemented: true`**。蛇形命名、int64 UTC ms、base64url 无填充、未知字段容忍（无 `additionalProperties: false`）。
- `README.md`：契约纪律 + §9「差异与裁决记录」列了 **25 条裁决**，其中 5 条仍 `待裁决`（`MetaReq.replace`、`Share S3/S8 响应`、`BATCH_REJECTED`、`X-Set-Mtime:0`、`SessionStartReq.client.caps`）。
- `.spectral.yaml`：最小 lint 规则集。

### 2.2 双端 codegen（`projects/phonefs/scripts/`）
- `gen-contract.sh` / `gen-contract.ps1`：等价入口（PS1 必须 UTF-8 BOM + CRLF）。
- `normalize_codegen_spec.py`：生成前写**临时副本**，去掉生成器无法表达的构造（`MetaReq.anyOf`、`details.additionalProperties:true`）；契约本体不改。
- `gen_error_codes.py`：从 `ErrorBody.code` enum 同源生成 Go/Kotlin 常量。
- 工具：Go `oapi-codegen v2.8.2`；`openapi-generator-cli 7.26.0.jar`（sha256 `1760050094997b9cc790cc1350be095f15da8c08a996b184d3eadc4ccda5e35e`，huaweicloud 镜像）。

### 2.3 Android 模块拆分
- `android/core` → 重命名为 **`android/core-server`**（全部服务端实现：PhoneFsServer / UploadStore / LoopbackTlsFront / FsBackend / MesFsBackend / Identity* / TlsKeyStores / SiteLocal / FsLog + 3 个测试）。
- 新建 **`android/core-protocol`**（纯 JVM，namespace `com.phonefs.core.protocol`）：`Dto.kt` / `Errors.kt` / `ErrCodes.gen.kt` / `fs/DocIds.kt` / 60 个生成模型（`src/generated/kotlin/.../protocol/gen/`）/ `src/test/.../GeneratedModelContractTest.kt`。
- `core-server` 改用 `api(project(":core-protocol"))`；`source` 改用 `implementation(project(":core-server"))`。
- `DocIds.kt`：`android.util.Base64` 换成 `java.util.Base64.getUrlEncoder().withoutPadding()`（删除了测试 shim `android/util/Base64.java`）。
- `settings.gradle.kts`：`include(":core-protocol")`、`":core-server"`、`":source"`。
- Kotlin 生成路径由旧 `android/core/src/generated` 改为 `android/core-protocol/src/generated/kotlin`。

### 2.4 消费者能力路线分析
- `docs/design/2026-11-02-phonefs-consumer-capability-roadmap.md`：C0→C1 只读→C2 分类/缩略图/seek→C3 写→C4 SSE/离线→C5 fleet→C6 大屏 fallback→C7 明确不做。竞品基线表、风险 R1–R10、验收门禁 W-C0~W-C6。**关键待拍板：SAF-only vs SAF-first + C6 fallback**。

## 3. 已知坑（避免重踩）

1. **openapi-generator 整数 enum bug**：整数 `enum:[0,1,2,3]` 会被生成为**字符串枚举**（`@SerialName("1")`），JSON 数字 `1` 解码抛 `JsonDecodingException`。契约已改用 `minimum/maximum`，`GeneratedModelContractTest.decodeAttrMediaTypeAsJsonNumber` 覆盖此回归。改契约时**不要**把 media_type 反向改回 enum 或 String。
2. **MetaReq.anyOf / details.additionalProperties**：生成器在 anyOf 存在时丢字段（空 data class），且 kotlinx IR 对 `Map<String,Any>` 失败 → 在 `normalize_codegen_spec.py` 临时副本里处理，契约本体保留。anyOf 删掉后「至少一字段」约束需服务端校验（已在 README 标注）。
3. **DocIds 解码变严格**：旧 `android.util.Base64` 容忍 `+`/`/`；新 `java.util.Base64.getUrlDecoder()` 拒绝 —— 旧 docId 若含非 url-safe base64 字符会返回 null。迁移期注意。
4. **分裂包 `com.phonefs.core.fs`**：`DocIds` 在 core-protocol，`FsBackend` 在 core-server，同包名跨模块。后续抽 core-client 时注意。
5. **生成文件仍 `??` 未跟踪**：要显式 `git add`（`go/internal/gen/`、`android/core-protocol/src/generated/`、`go/internal/fsprovider/error_codes.gen.go` 等）。
6. **AGP 9 弃用警告**：`kotlin.srcDir` 弃用提示，无害。

## 4. 验证（2026-11-02 主会话独立复跑，全绿）

| 项 | 命令 | 结果 |
|---|---|---|
| 模块引用无陈旧 `:core` | grep `project(":core")` / `:core:` / `android/core/` | **0 命中** |
| settings 包含 | `settings.gradle.kts` | `:core-protocol` / `:core-server` / `:source` ✓ |
| codegen 零 diff | `scripts/gen-contract.sh` ×2，sha256 比对 | GO+ErrCodes PASS、KT 60 文件 PASS |
| core-protocol 测试 | `:core-protocol:testDebugUnitTest` | **2/2**（GeneratedModelContractTest） |
| core-server 测试 | `:core-server:testDebugUnitTest` | **24/24**（Content 8 + Write 11 + UploadStore 5） |
| source 构建 | `:source:assembleDebug` | ✓ BUILD SUCCESSFUL |
| Go 构建 | `go build ./...` | PASS |
| Go vet | `go vet ./...` | PASS |

本机工具链（接手会话必备）：Go 1.26.2 amd64 `@C:\Programs\go-amd64\bin\go.exe`（`GOPROXY=https://goproxy.cn,direct`）；JDK 21 `@C:\Programs\Java\jdk-21.0.11`；Kotlin 2.4.21 / AGP 9.5.0 / Gradle 9.8.2。Windows：路径 `C:/Code/...`；adb 前 `export MSYS_NO_PATHCONV=1`。

## 5. 待办（C0 之后，未被授权，未开始）

> 治理红线：`.spectral.yaml`/`contract/`、`docs/*-spec.md`、`agents/`、`README.md` 属保护路径，改动需人工 review。M3 分支**未合 main，合入/推送需用户显式授权**。

1. **C0 commit**：在 `feat-phonefs-m3` 提交（Conventional Commits，`feat(phonefs):` / `docs:`）。生成文件 + 拆分 + 文档一起入。
2. **M3 合 main**：Colossus 复审 P0=0 → 用户授权 → 合入推送。M3 仍停在 `feat-phonefs-m3`，未合（见 memory `phonefs-selfbuild-status.md`）。
3. **服务端补 10 个 P0 端点**（`pair/hello`、`pair/commit`、`pair/sessions/{sid}`、`session/start|ping|{sid}`、`fs/stat`、`index/meta`、`index/snapshot`、`events`）+ P1（`thumb`、`index/changes`、`fs/copy`）—— 消费端 P0 才能按设计落地。
4. **抽 `core-client`**：OkHttp + TLS pin + Range + 上传 + SSE + session + vault + NSD（依赖 `core-protocol`，不依赖 `core-server`）。
5. **`consumer` 模块**：SAF `DocumentsProvider` + 配对/设备/设置 UI；C6 fallback 需产品拍板 SAF-only vs SAF-first。
6. **手写 DTO/Errors 迁移到生成模型**：分阶段，不在 C0 范围。
7. **§9 待裁决项**需 ADR / SPEC 文字修订闭环（5 条待裁决）。

## 6. 接手第一步（建议）

```bash
cd C:/Code/Wideawake-wt-phonefs-m3/projects/phonefs
export GO_BIN="/c/Programs/go-amd64/bin/go.exe"
export JAVA_HOME="C:/Programs/Java/jdk-21.0.11"
export MSYS_NO_PATHCONV=1

# 1) 复现 codegen 零 diff
bash scripts/gen-contract.sh          # 跑两次，sha256 应一致
# 2) 复现测试
cd android && ./gradlew.bat :core-protocol:testDebugUnitTest :core-server:testDebugUnitTest :source:assembleDebug
cd .. && (cd go && "$GO_BIN" build ./... && "$GO_BIN" vet ./...)
```

若全绿，C0 已可 commit；后续按 §5 推进。任何偏离上表的结果都是回归，先排查再动代码。

---

## 附：模块依赖图（目标态）

```
android/
  core-protocol/   # DTO / ErrCode / DocIds / 协议常量（纯 JVM）        ← 新增
  core-client/     # OkHttp + TLS pin + Range + 上传 + SSE + session     ← 未建
  core-server/     # 现有 server / upload / TLS front / MES backend      ← 由 core 重命名
  source/          # 手机源端 App                                       （行为不变）
  consumer/        # SAF provider + 配对/设备/设置 UI (+ C6 fallback)   ← 未建
依赖：consumer → core-client → core-protocol；source → core-server → core-protocol
consumer 不依赖 core-server。
```
