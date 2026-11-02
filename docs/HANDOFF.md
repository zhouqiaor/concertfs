# PhoneFS 本会话交接总文档（M3 硬化 + Phase 0 契约 + C0 拆模块 + 消费端路线）

日期：2026-11-02
分支：`feat-phonefs-m3`（worktree `C:/Code/Wideawake-wt-phonefs-m3`）
**合入状态**：分支领先 `main` **14 个 commit**（M3 硬化已 commit）；Phase 0 契约 + C0 拆模块 + 消费端分析**全部未 commit，落盘在 working tree**。详见各节「未提交清单」。
**红线**：`contract/`、`docs/*-spec.md`、`agents/`、`README.md` 属保护路径，改动需人工 review；**M3 未合 main，合入/推送 main 需用户显式授权**。

> 本文件 + 子文档 [HANDOFF-C0.md](HANDOFF-C0.md) 覆盖本会话全部任务。所有结论均可被下面命令独立复现。

---

## 0. 全景状态速查

| 任务 | 范围 | commit 状态 | 验证 | 下一步 |
|---|---|---|---|---|
| **M3 硬化** | 流式 GET / 写落盘 / delta / 原子 rename / CIO 回切 / P0-1 修复 / keep-alive / root 保护 / ADR-19 | 已 commit（14 个，2 个标 `[需人工 Colossus 复核]`） | 真机 + 单测全绿 | Colossus 复审 P0=0 → 用户授权 → 合 main 推送 |
| **Phase 0 契约** | OpenAPI 3.1 单文件 + README 裁决 + 双端 codegen | 未 commit（untracked） | codegen 零 diff、Go build/vet 绿 | commit + 待裁决项 ADR 闭环 |
| **C0 拆模块** | `:core` → `:core-server` + 新 `:core-protocol` | 未 commit（rename+untracked+modified） | 26/26 测试绿、codegen 零 diff | commit |
| **消费端路线分析** | `docs/design/...consumer-capability-roadmap.md` | 未 commit（untracked） | 分析稿，非代码 | 产品拍板 SAF-only vs fallback |

分支详情：`git log --oneline main..HEAD`（14 个）。最近的真实提交 HEAD=`8eed103`（ADR-19 收尾）。

---

## 1. M3 硬化（已 commit，待合 main）

### 1.1 做了什么
切片（已 ff 进分支）：
- 流式大文件 GET（`a6e6401` 固定缓冲流式；`7534e7b` channel 背压）。
- 写映像落盘 + 增量脏块刷新（`3556934` 落盘 + 增量；`0c8ca7a`/`7534e7b` 引擎回切）。
- delta 上传（`X-Base-ETag`/`X-Upload-Final`，412 并发保护）+ 原子 rename（PATCH `replace`=ATOMIC_MOVE+REPLACE_EXISTING）（`297e9df`）。
- **根因修复**：Netty 环回引擎在真机大流上 RSS 无界（2.1GiB 交付后 RSS 4.7–7.3GiB 被杀）→ 回切 CIO 3.6.1（TLS 已由 LoopbackTlsFront 承担）（`0c8ca7a` + `fa28728`/`8eed103` ADR-19）。
- **P0-1 修复**（`5957614`）：同句柄周期 Open(TRUNC)→Write→SetFileTime 时 mtime 延后到上传 `X-Set-Mtime`，保 delta 基线不失效；fake server 补回归 `TestUtimensDuringOpenWriteKeepsDeltaBaseline`。
- keep-alive（`e47533d` 复用连接）。
- root 保护（`6d91fd9` 拒绝经编码 path id 抵达存储根）。

### 1.2 真机证据
- `projects/phonefs/test/m3/cio-largeget-evidence.txt`：2.1GiB HTTPS 全量 GET sha 双端一致（`818e2054…81ab`）、84s/24.4MiB/s、VmRSS 162–178MiB、HWM 188MiB、keep-alive 第二请求 connects=0。
- 终轮构建回归：W2 41/41、M2 20/20、M3 12/12(+1 ENV_LIMIT)、rw 15/15、ro 16/16、core JVM 23/23、Go test/vet/nocgo 绿。

### 1.3 门禁与卡点
- 2 个 commit 标 `[需人工 Colossus 复核]`（`fa28728`、`8eed103`）—— ADR-19 改了生产引擎（Netty 退役），属保护路径 SPEC 增补，**必须 Colossus 复审 P0=0** 才能合 main。
- **M3 未合 main**：领先 14 commit，合入/推送需用户显式授权。

### 1.4 接手续做
1. 跑 Colossus 门禁：`agents/colossus/skills/ocr-review.md`，目标 P0=0，标记 `review: ocr pass [nP0/nP1/nP2]`。
2. 用户授权后：rebase 最新 main → 合入 → 推送 `zhouqiaor`（AGENTS.md 治理：main 门禁全绿自动合入）。
3. 设备已复原（P: 卸载、source force-stop、18443 关、临时 token/APK/夹具删除），复跑前需重新部署。

---

## 2. Phase 0 契约（未 commit，保护路径）

### 2.1 未提交清单（untracked）
- `projects/phonefs/contract/openapi/phonefs.yaml`：OpenAPI 3.1.0 单文件。30 path / **33 operation**（基础 22 = P0 19 + P1 3 + Fleet(ADR-18) 3 + Share(ADR-17) 8）。45 错误码（含 `PRECONDITION_FAILED` 412；`ANOTHER_SESSION` deprecated）。**9 个 `x-phonefs-implemented: true`**。蛇形命名、int64 UTC ms、base64url 无填充、未知字段容忍。
- `projects/phonefs/contract/README.md`：**25 条裁决**，5 条仍 `待裁决`（`MetaReq.replace`、`Share S3/S8 响应`、`BATCH_REJECTED`、`X-Set-Mtime:0`、`SessionStartReq.client.caps`）。
- `projects/phonefs/contract/.spectral.yaml`。
- `projects/phonefs/scripts/gen-contract.{sh,ps1}`、`normalize_codegen_spec.py`、`gen_error_codes.py`、`tools/`（含 openapi-generator jar）。
- `projects/phonefs/go/internal/gen/phonefs.gen.go`（1566 行，无 runtime 依赖）、`go/internal/fsprovider/error_codes.gen.go`（生成）。
- `projects/phonefs/android/core-protocol/src/generated/kotlin/`（60 模型，见 §3）。

### 2.2 契约纪律（接手必读）
- OpenAPI 是唯一真源；Go `oapi-codegen v2.8.2` / Kotlin `openapi-generator 7.26.0` 生成 DTO；**禁止手写业务 DTO**。
- additive minor；无 `additionalProperties: false`；成功不包 envelope；错误码 enum 只定义一次。
- 变更流程（README §10）：先改 yaml → 补 ADR（若触保护路径）→ 重跑 codegen 零 diff → conformance → lint/结构校验。
- 生成前 `normalize_codegen_spec.py` 写临时副本，去掉 `MetaReq.anyOf` 与 `details.additionalProperties:true`（契约本体不改）。

### 2.3 已知坑
- **整数 enum bug**：`enum:[0,1,2,3]` 被生成为字符串枚举，JSON 数字解码失败 → 契约改用 `minimum/maximum`，有回归测试 `GeneratedModelContractTest.decodeAttrMediaTypeAsJsonNumber`。不要反向改回 enum/String。
- 生成文件需显式 `git add`。

---

## 3. C0 拆模块（未 commit，独立复验全绿）

> 深度细节见 [HANDOFF-C0.md](HANDOFF-C0.md)。此处给结论与未提交清单。

### 3.1 改动
- `android/core` → 重命名 `android/core-server`（全部服务端实现 + 3 测试）。
- 新建 `android/core-protocol`（纯 JVM，namespace `com.phonefs.core.protocol`）：`Dto.kt`/`Errors.kt`/`ErrCodes.gen.kt`/`fs/DocIds.kt`/60 生成模型/`GeneratedModelContractTest.kt`。
- `core-server api(:core-protocol)`、`source implementation(:core-server)`、`settings.gradle.kts` 含 `:core-protocol`/`:core-server`/`:source`。
- `DocIds` 换 `java.util.Base64`（删测试 shim `android/util/Base64.java`）。
- modified：`android/settings.gradle.kts`、`source/build.gradle.kts`、`.gitignore`、`README.md`。

### 3.2 未提交清单（rename + untracked + modified，见 §0 git status）
- 23 个 `R`/`RM` 重命名（`core/**` → `core-server/**` + `core-protocol/**`）。
- `D android/core/src/test/java/android/util/Base64.java`。
- `?? android/core-protocol/build.gradle.kts`、`?? android/core-protocol/src/generated/`、`?? android/core-protocol/.../ErrCodes.gen.kt`、`?? android/core-protocol/src/test/`。

### 3.3 独立复验（2026-11-02 主会话，全绿）
| 项 | 结果 |
|---|---|
| 陈旧 `:core` 引用（grep） | 0 命中 |
| codegen 零 diff（跑两次 sha256） | GO+ErrCodes PASS、KT 60 文件 PASS |
| `:core-protocol:testDebugUnitTest` | 2/2 |
| `:core-server:testDebugUnitTest` | 24/24（Content 8 + Write 11 + UploadStore 5） |
| `:source:assembleDebug` | BUILD SUCCESSFUL |
| Go `build ./...` + `vet ./...` | PASS |

### 3.4 坑
整数 enum（§2.3）、anyOf/additionalProperties 在 normalize 副本处理、DocIds 解码变严格（拒 `+`/`/`）、分裂包 `com.phonefs.core.fs` 跨模块、AGP9 `kotlin.srcDir` 弃用警告无害。

---

## 4. 消费端能力路线分析（未 commit，分析稿）

- `docs/design/2026-11-02-phonefs-consumer-capability-roadmap.md`：C0→C1 只读→C2 分类/缩略图/seek→C3 写→C4 SSE/离线→C5 fleet→C6 大屏 fallback→C7 明确不做。竞品基线表、风险 R1–R10、验收 W-C0~W-C6。
- **关键待拍板：SAF-only vs SAF-first + C6 fallback**（车机/Android-x86 无 DocumentsUI 时「装了看不见」）。
- 现状：无 `:consumer`、服务端 9/22 路由、core 未拆（C0 已解决 core 拆分部分）。
- 消费端 P0 落地前需服务端补 10 个 P0 端点（§5 待办 #3）。

---

## 5. 统一待办（按依赖顺序）

1. **commit C0 + Phase 0 契约**（同分支 `feat-phonefs-m3`，Conventional Commits：`feat(phonefs):`/`docs:`/`chore:`）。生成文件 + 拆分 + 文档一起入。保护路径 commit 需人工 review 标记。
2. **M3 合 main**：Colossus 复审 P0=0（针对 ADR-19 两 commit）→ 用户授权 → rebase 最新 main → 合入 → 推送 `zhouqiaor`。
3. **服务端补 10 个 P0 端点**：`pair/hello`、`pair/commit`、`pair/sessions/{sid}`、`session/start|ping|{sid}`、`fs/stat`、`index/meta`、`index/snapshot`、`events`；P1：`thumb`、`index/changes`、`fs/copy`。
4. **抽 `core-client`**：OkHttp + TLS pin + Range + 上传 + SSE + session + vault + NSD（依赖 `core-protocol`，不依赖 `core-server`）。
5. **建 `consumer` 模块**：SAF `DocumentsProvider` + 配对/设备/设置 UI；C6 fallback 待产品拍板。
6. **手写 DTO/Errors 迁移到生成模型**：分阶段，不在 C0 范围。
7. **§2.1 README §9 待裁决项**需 ADR / SPEC 文字修订闭环（5 条待裁决）。

---

## 6. 接手第一步（验证环境）

```bash
cd C:/Code/Wideawake-wt-phonefs-m3/projects/phonefs
export GO_BIN="/c/Programs/go-amd64/bin/go.exe"
export JAVA_HOME="C:/Programs/Java/jdk-21.0.11"
export MSYS_NO_PATHCONV=1          # adb/斜杠路径前必设

# 复现 codegen 零 diff（跑两次）
bash scripts/gen-contract.sh

# 复现 Android 测试 + 构建
cd android && ./gradlew.bat :core-protocol:testDebugUnitTest :core-server:testDebugUnitTest :source:assembleDebug

# 复现 Go
cd .. && (cd go && "$GO_BIN" build ./... && "$GO_BIN" vet ./...)

# 查看分支领先 main 的 commit
cd ../.. && git log --oneline main..HEAD
```

本机工具链：Go 1.26.2 amd64 `@C:\Programs\go-amd64\bin\go.exe`（`GOPROXY=https://goproxy.cn,direct`）；JDK 21 `@C:\Programs\Java\jdk-21.0.11`；Kotlin 2.4.21 / AGP 9.5.0 / Gradle 9.8.2。Windows：路径 `C:/Code/...`；adb 前 `export MSYS_NO_PATHCONV=1`；真机操作前校验 `ro.serialno==HOMEVISION202111`。

任何偏离上表的结果都是回归，先排查再动代码。

---

## 附：模块依赖图（目标态）
```
android/
  core-protocol/   # DTO / ErrCode / DocIds / 协议常量（纯 JVM）        ← 新建（§3）
  core-client/     # OkHttp + TLS pin + Range + 上传 + SSE + session     ← 未建（§5 #4）
  core-server/     # 现有 server / upload / TLS front / MES backend      ← 由 core 重命名（§3）
  source/          # 手机源端 App                                       （行为不变）
  consumer/        # SAF provider + 配对/设备/设置 UI (+ C6 fallback)   ← 未建（§5 #5）
依赖：consumer → core-client → core-protocol；source → core-server → core-protocol
consumer 不依赖 core-server。
```
