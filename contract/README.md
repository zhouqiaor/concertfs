# PhoneFS 契约（Phase 0）

本目录是 PhoneFS 双端协议的机器可读真源所在。唯一权威文件：

- `openapi/phonefs.yaml`：OpenAPI 3.1.0，单文件自包含。

SPEC `docs/design/2026-10-31-phone-files-selfbuild.md` §6 的文字与 yaml 冲突时，以 yaml 为准；若 yaml 需要变更，按本文末尾“变更流程”执行，不得只改一端实现。

## 1. 契约纪律

1. **OpenAPI 是唯一真源**（SPEC §6 / ADR-16）。Go 端 `oapi-codegen`、Kotlin 端 `openapi-generator 7.x` 从本 yaml 生成 DTO/client；禁止手写业务 DTO（生成物之外只允许 mapper/扩展）。
2. **additive minor**：不新增必填字段、不改变既有字段语义、不换路径。新增能力用可选字段/可选端点/`features` 协商位表达。
3. **未知字段容忍**：所有对象都不设置 `additionalProperties: false`；客户端必须忽略不认识的字段与不认识的枚举扩展值（枚举比较应按“已知值 + 未知值透传”处理）。
4. **成功不包 envelope**：成功响应直接返回业务对象；失败统一 `{"error":{...}}`。
5. **错误码同源**：错误码 enum 只在 `ErrorBody.code` 定义一次；`scripts/gen_error_codes.py`（M1 落地）从该 enum 生成 Go/Kotlin 常量。客户端合成码不进入 HTTP enum。
6. **阶段与实现标记**：
   - `x-phonefs-stage: P0 | P1`：交付阶段。
   - `x-phonefs-implemented: true | false`：当前实现是否已挂载。真源是 `projects/phonefs/android/core-server/src/main/kotlin/com/phonefs/core/net/PhoneFsServer.kt` 的实际路由，而不是 SPEC 是否定义了端点。
   - `x-phonefs-origin: ADR-17 | ADR-18`：非基础 SPEC 端点的来源 ADR。

## 2. 命名、时间与二进制约定

| 项 | 约定 |
|---|---|
| 字段名 | `snake_case`（wire 名即契约名；Go json tag / Kotlin `@SerialName` 同名） |
| 时间 | 一律 `int64` UTC unix **毫秒**（`*_ms`）；HTTP 头 `Last-Modified` 除外，按 HTTP-date |
| 二进制字段 | base64url、**无填充**（`A-Za-z0-9_-`），如证书 DER、ECDH 公钥点、token |
| 内容正文 | 例外：`/v1/fs/content`、`/v1/share/offers/{oid}/content/{fid}`、`/v1/thumb` 的 body 是原始字节或 JPEG，不走 base64 |
| 布尔 query | `?download` 语义为“存在即 attachment”；见 §9 裁决 |
| 大小/容量 | `int64` 字节；`block_size` 也是 int64 |
| 分页游标 | 不透明字符串；`fs/list` 用空串表示结束，`index/snapshot` 用 `null` 表示结束（沿用 SPEC 原文） |
| revision | 单调 `int64`；`fs/list.revision` 当前实现为当页 `max(modified_ms)`，见 §9 |

### docId

冻结形态（契约 `DocId.pattern` 刻意保持宽松，分类会扩展）：

| 形态 | 含义 |
|---|---|
| `root` | 虚拟根，固定 8 行 = 7 个 `cat:*` + `stor:primary` |
| `cat:image` `cat:video` `cat:audio` `cat:document` `cat:archive` `cat:downloads` `cat:screenshots` | P0 七个分类 |
| `cat:camera` | P1；不进入 `/v1/roots` 固定行，仅当 `session/start` 协商 `features.camera=true` 时由客户端本地渲染为第 9 root |
| `stor:primary` | 内部存储根（`/storage/emulated/0`） |
| `p:<base64url(UTF-8 绝对路径)>` | 真实文件/目录；无填充。除调试展示外，客户端不得拼接路径语义 |

docId 上限 1024 字符。

## 3. 端点与阶段

统计：**33 个 operation / 30 个 path item**（`/v1/fs/content` 的 GET+PUT、`/v1/pair/devices/me` 的 GET+DELETE、`/v1/share/offers/{oid}` 的 GET+DELETE 共用一个 path item，因此 path item 数比 operation 数少 3）。

| 组 | operation 数 | 阶段 |
|---|---|---|
| 基础 SPEC 22 端点 | 22 | P0 19 / P1 3（copy、thumb、index/changes） |
| Fleet（ADR-18） | 3 | P0 |
| Share（ADR-17） | 8 | P1（按 Phase 0 任务书；见 §9 裁决） |

### 当前实现已挂载（`x-phonefs-implemented: true`，9 个）

`pairInfo`、`getRoots`、`listDir`、`statFs`、`getContent`、`createEntry`、`putContent`、`patchMeta`、`deleteEntry`。

其余 24 个 operation 均已定义 schema，但标记为 `false`。未实现不等于契约可改：客户端仍应按契约预生成 DTO，服务端就绪后只补实现，不改 wire 形态。

## 4. 安全与服务器

- 全局 `security: bearerAuth`。
- `security: []` 仅 4 个配对端点：`GET /v1/pair/info`、`POST /v1/pair/hello`、`POST /v1/pair/commit`、`DELETE /v1/pair/sessions/{sid}`。
- 认证头：`Authorization: Bearer <32B base64url token>` + `X-Device-Id`。
- `servers[0].url = https://{host}:{port}`；默认 `host=192.168.43.1`、`port=18443`。SPEC §6.1 把端口定义为“动态端口”，`18443` 只是手机实现的首选端口，实际端口以 mDNS TXT / 扫码载荷为准。Go client 的 `DefaultBaseURL` 同样默认 18443。见 §9 裁决。

## 5. 头字段的集中定义

请求头在 `components/parameters`（Parameter Object 需要 `name`/`in`），响应头在 `components/headers`：

- 认证/追踪：`Authorization`、`X-Device-Id`、`X-Trace-Id`
- 配对：`X-Pair-MAC`
- 读：`Range`、`If-Range`
- 写：`Content-Length`、`Content-Range`、`X-Upload-Id`、`X-Base-ETag`、`X-Upload-Final`、`X-Set-Mtime`
- 表述：`Accept-Language`、`Accept-Encoding`、`Last-Event-ID`
- 响应：`ETag`、`Last-Modified`、`Accept-Ranges`、`Content-Disposition`、`Content-Range`、`Content-Length`、`Cache-Control`、`Retry-After`

## 6. 错误包络

```json
{"error":{"code":"INVALID_ARGUMENT","message":"...","trace_id":"...","retriable":false,"details":{}}}
```

- `details` 是 object，`additionalProperties: true`，**不设 required**。
- 已知键（按错误码出现，客户端必须容忍未知键）：`received`、`required`、`expected`、`actual`、`deleted`、`failed`、`reason`、`hint`、`latest_full`、`size`、`retry_after`、`active`、`max`、`fid`、`free`、`plan`。
- 错误码 enum 共 45 个值，覆盖 SPEC §6.6 冻结表 + ADR-17/18 增量 + `PRECONDITION_FAILED(412)`。
- `ANOTHER_SESSION` 保留为 deprecated 别名。OpenAPI 3.1 没有“单个 enum 值 deprecated”的标准标注，契约用 `x-phonefs-deprecated-codes: {ANOTHER_SESSION: {deprecated: true}}` 表达，README 同时记录。

### 客户端合成码（不产生 HTTP 响应，不在 enum）

| code | 来源 |
|---|---|
| `OFFLINE` (599) | SPEC §6.6 |
| `PAIR_SAS_MISMATCH` | SPEC §6.6（SAS 目视不一致；无请求发出） |
| `FINGERPRINT_CHANGED` | SPEC §6.6（TLS 握手层拒绝） |
| `LOCAL_DISK_FULL` | Share 衍生设计 §3.3 |
| `SENDTO_NO_RW_DEVICE` | Share 衍生设计 §3.3 |
| `STAGING_QUOTA_EXCEEDED` | Share 衍生设计 §3.3 |

Go client 内部还会在“服务端忽略 Range 返回 200”的防御分支合成 `RANGE_NOT_SATISFIED`（注意不是 wire 上的 `RANGE_NOT_SATISFIABLE`）。这是 client 内部错误对象，不是协议码。

## 7. SSE 事件（`GET /v1/events`）

`Content-Type: text/event-stream`；每 25s 一行 `: ping`。`x-sse-events` 扩展列出事件名与 data schema：

| event | data |
|---|---|
| `index` | `{revision}` |
| `state` | `{state}`（如 `sharing_stopping`） |
| `storage` | `{total,free}`（5s 节流） |
| `share_offer` | `{offer_id,from_device,file_count,total_bytes}` |
| `share_batch` | `{batch_id,state}`（canceled / receiving / complete） |
| `device_scope` | `{scope}`（ro / rw） |
| `device_revoked` | `{reason}`（revoked / wipe_all） |
| `fleet` | `{active,max,writers,writer_id8?}` |

降级：`?minimal=1` 立即返回 `MinimalState`（`{revision,state,storage}`），客户端每 10s 轮询。`Last-Event-ID` 仅作提示，服务端无状态、不补发。

## 8. 生成与校验命令

入口（SPEC §12.3 冻结的 `just gen` / `scripts/gen-contract.sh`，Phase 0 已落地；两条入口等价）：

```bash
# bash / Git Bash（Windows 或 Linux）
projects/phonefs/scripts/gen-contract.sh

# Windows PowerShell
powershell -ExecutionPolicy Bypass -File projects/phonefs/scripts/gen-contract.ps1
```

脚本内部等价于（路径相对仓库根；`scripts/tools/` 为本地工具缓存，已忽略）：

```bash
# 0. 生成前规范化：只写临时副本，契约本体不改
NORM=projects/phonefs/scripts/tools/.codegen/phonefs.normalized.yaml
python projects/phonefs/scripts/normalize_codegen_spec.py \
  projects/phonefs/contract/openapi/phonefs.yaml "$NORM"

# Go models（oapi-codegen v2.8.2）
oapi-codegen -generate types -package gen \
  -o projects/phonefs/go/internal/gen/phonefs.gen.go \
  "$NORM"

# Kotlin models（openapi-generator 7.26.0, kotlinx.serialization）
java -jar projects/phonefs/scripts/tools/openapi-generator-cli-7.26.0.jar generate \
  -i "$NORM" \
  -g kotlin \
  --type-mappings=object=JsonObject \
  --import-mappings=JsonObject=kotlinx.serialization.json.JsonObject \
  --additional-properties=serializationLibrary=kotlinx_serialization,enumPropertyNaming=UPPERCASE,modelPackage=com.phonefs.core.protocol.gen,sourceFolder=,openApiNullable=false \
  --global-property=models,modelDocs=false,modelTests=false,supportingFiles=false \
  -o projects/phonefs/android/core-protocol/src/generated/kotlin

# 错误码常量
python projects/phonefs/scripts/gen_error_codes.py
```

生成前脚本会用 `scripts/normalize_codegen_spec.py` 在临时目录写出契约副本（不改契约本体），
去掉生成器无法表达的构造；细节见脚本头注释。

本机最小校验（Python 3.13 + PyYAML；本机 PyYAML 位于 user-site，`-I` 隔离时需显式追加路径）：

```bash
python -I -c "import sys; sys.path.insert(0, r'C:\Users\georgeslark\AppData\Roaming\Python\Python313\site-packages'); import yaml; yaml.safe_load(open(r'projects/phonefs/contract/openapi/phonefs.yaml', encoding='utf-8'))"

# 若环境把 PyYAML 装在系统 site-packages，可直接：
python -I -c "import yaml; yaml.safe_load(open(r'projects/phonefs/contract/openapi/phonefs.yaml', encoding='utf-8'))"
```

结构校验：临时脚本检查所有内部 `$ref` 可解析、每个 operation 有 `operationId`/`tags`/`responses`/`security`、operation 数 = 33、已实现集合 = 9。脚本不留在仓库。

Lint：`.spectral.yaml`（最小规则集）。未安装 lint 也可提交配置，安装后运行：

```bash
spectral lint projects/phonefs/contract/openapi/phonefs.yaml
```

## 9. 与 SPEC / 实现 / 衍生设计的差异与裁决记录

以下“裁决”是 Phase 0 契约落位时按任务书与现有实现做的取舍；标注“待裁决”的项需要后续 ADR 或 SPEC 文字修订。

1. **`Attr.parent` 可选 nullable（裁决）**：SPEC §6.2 把 `parent` 写成必填；实际 Kotlin `Attr.parent: String? = null`，且虚拟/边界条目可为 null。契约按可选 `type: [string, "null"]` 落位，避免强制实现伪造父 id。
2. **`Attr.media_type` 为 integer 0..3（裁决）**：SPEC 定义 `media_type` 为整数（0 unknown / 1 image / 2 audio / 3 video）。当前 Kotlin DTO 误写为 `String?` 且实现恒 `null`。契约按 SPEC 的 integer，Kotlin 侧需在 codegen 迁移时修正；**不要**按现有 String 反向改契约。整数范围用 `minimum/maximum` 而不是 `enum`：openapi-generator 7.26.0 会把整数 enum 生成为字符串枚举（`@SerialName("1")`），JSON 数字 `1` 无法解码；`GeneratedModelContractTest` 覆盖该回归。
3. **`MetaReq.replace` 为实现者增量（待 ADR）**：SPEC §6.5.8 没有 `replace` 字段；`PhoneFsServer.kt` 已实现“目标存在时替换”语义，`client_write.go` 也已发送。契约把它标为可选 boolean、默认 false，并加 `x-phonefs-origin: implementer`；需要补 ADR 或从实现移除。
4. **`fs/list.revision` 语义（裁决）**：SPEC 说“当前索引 revision”；Kotlin 实现返回当页 `max(modified_ms)`（root 固定行返回 0）。契约保留 int64 字段与 SPEC 语义描述，同时在 description 记录实现现状。M1 索引后端接入后应改为真实 revision。
5. **`fs/list.cursor` 已定义但 W2 未实现（裁决）**：实现遇到非空 `cursor` 一律 `400 BAD_CURSOR`。契约保留 cursor/next_cursor 分页定义（SPEC 冻结），实现按 P1 补齐；`x-phonefs-implemented: true` 只表示端点已挂载，不表示分页已完整。
6. **`pair/info.pairing_open` 当前恒 false（实现现状）**：W2 固定演示配对，配对窗未开放。契约字段仍是 boolean，不把 false 固化为常量。
7. **配对端点安全豁免的实现缺口**：SPEC 规定 4 个配对端点无 bearer；`PhoneFsServer.kt` 当前只挂载 `GET /v1/pair/info`，且拦截器只豁免该路径。将来挂载 hello/commit/sessions 时必须同步扩展豁免列表。
8. **Share 阶段标 P1（裁决）**：Share 衍生设计 §3.1 写“P0 8 / P1 0”，Phase 0 任务书要求 Share 8 端点标 `x-phonefs-stage: P1`。契约按任务书标 P1；如需改回 P0，只需改 8 个 operation 的 `x-phonefs-stage`，不涉及 schema。
9. **Share S3 成功响应体（待裁决）**：Share 草案只规定 accept 幂等、返回当前状态，未冻结具体 body。契约暂定返回 `ShareOfferDetail`。
10. **Share S8 cancel 成功响应（待裁决）**：草案只写“幂等”，未规定 200 还是 204。契约暂定 `204 No Content`。
11. **Share `BATCH_REJECTED`（待裁决，未收录）**：Share 草案 §3.2 示例出现 `409 {"code":"BATCH_REJECTED"}`，但 ADR-17/§3.3 冻结错误码表没有该码。契约不把它加入 enum；`POST /v1/share/batches` 的 409 response 在 description 中保留说明。若确认需要，必须 add enum + ADR，而不是让实现私自发码。
12. **`If-Match` 写回与 `PUT /v1/fs/leases` 不收录（裁决）**：两者都只出现在 Multidev §3.6 的 P1 草案，未进 P0，也未在 ADR-18 冻结端点表。契约不定义 `PUT /v1/fs/leases`、不在写端点声明 `If-Match`；草案里的 `423 LEASE_HELD` 同样不进 enum。`412 PRECONDITION_FAILED` 仅用于已实现的 delta 上传 `X-Base-ETag`。
13. **端口（裁决）**：SPEC §6.1 说动态端口；实现首选 18443、占用则回退动态端口；Go client 默认 18443。契约 servers 用变量默认 18443，并在 description 注明动态端口才是权威。
14. **配对载荷的 base64 vs base64url（裁决）**：SPEC §6.4 文字写 `<base64 DER>`、`<P-256 点 base64>`；SPEC §6.1 全局约定“二进制字节一律 base64url（无填充）”。契约按全局约定使用 base64url 无填充，§6.4 文字应同步修订。
15. **`download` query 歧义（裁决）**：SPEC 写“`download` 给出时带 `Content-Disposition`”；实现是“query 参数存在即 attachment”，不解析值。契约定义为 boolean，description 明确“存在即 attachment”；客户端应发 `?download=1` / `?download=true`，不要依赖裸 `?download`。
16. **`X-Set-Mtime: 0` 语义冲突（待裁决）**：Kotlin server 接受 0 并会设置 epoch 0；Go client 只在 `mtimeMs > 0` 时发送，因此 0 实际不可表达。契约 `minimum: 0` 保留合法值，但需要统一“0 是否等于缺省 now”。
17. **`index/snapshot` 与 `fs/list` 的结束游标不一致（裁决）**：SPEC 原文分别是 `next_cursor: null` 与 `next_cursor: ""`。契约不改写，分别保持 nullable 与空串。
18. **`index` schema 1→2（裁决）**：ADR-21 已接受 schema:2 的 5 个可选列（`title/artist/album/app_label/duration_ms`）。契约把 `schema` 定义为 enum [1,2]，新列全部 optional；旧客户端靠未知字段容忍。
19. **`SessionStartReq.client.caps`（待裁决，未收录）**：Multidev §4.1 示例含 `client.caps`，但 ADR-18 端点表与 Phase 0 任务书只要求 `kind/version/os/name?`。契约暂不定义 caps，避免把示例当冻结字段。
20. **`SessionFeatures` 新三位的必填性（裁决）**：SPEC 时代的 `thumb/watch/changes/seek/copy/camera` 标 required；ADR-17/18 新增的 `share/fleet/mirror` 标 optional，以便旧服务端响应仍可被新客户端解析。
21. **`/v1/roots` 路径（澄清）**：`client.go` 注释说“SPEC 草案 /v1/fs/roots”是陈旧注释；当前 SPEC §6.3 与实现都是 `/v1/roots`。契约按 `/v1/roots`。
22. **`ANOTHER_SESSION` 的 deprecated 表达（裁决）**：见 §6；OpenAPI 无 per-enum deprecated 标准，使用 `x-phonefs-deprecated-codes`。
23. **`TOO_LARGE` 的 413 行**：Share 衍生设计把 `TOO_LARGE` 写成“既有码，补 share 语义”；SPEC §6.6 413 行已有 `TOO_LARGE`，契约不重复定义。
24. **errors enum 的 45 个值**：SPEC 冻结表 400–507（含 deprecated 别名）+ 增量登记（DEVICE_MISMATCH/WRITER_BUSY/FLEET_FULL/Share 码）+ `PRECONDITION_FAILED`。`NOT_READY`/`UNAVAILABLE` 保持既有码，不因 Share 另起新码。
25. **内容端点 `Content-Type`（裁决）**：SPEC §6.5.4 写“`Content-Type`（mime，octet-stream 兜底）”；Phase 0 任务书要求 `application/octet-stream`。契约的 200/206 response 只声明 `application/octet-stream`，便于 codegen 与二进制处理；服务端实现仍可按 mime 返回，客户端不得依赖该头判断内容类型（`Attr.mime` 才是元数据真源）。若后续需要严格表达 mime，应把 200/206 改为 `*/*` 或补 `Content-Type` 响应头 schema，不能只改实现。

## 10. 变更流程

1. 先改 `openapi/phonefs.yaml`（schema/operation/header/enum 任一变更都算契约变更）。
2. 若改动触碰 SPEC §6 冻结语义、保护路径或 ADR 已冻结的决策，先补 ADR（additive 标注，不改写旧条目），再改 yaml。
3. 重跑 codegen（`just gen` 或 `scripts/gen-contract.sh`），确认 Go/Kotlin 生成物零手工 diff；错误码常量由 `scripts/gen_error_codes.py` 同源再生成。
4. 跑四边 conformance（Go srv/cli × Kotlin srv/cli）与固定向量（SAS/指纹）；新增可选字段必须覆盖“有值/缺省/旧客户端忽略”三态。
5. 跑 lint（`.spectral.yaml`）与结构校验（`$ref` 全部可解析、operation 元数据齐全）。
6. `contract/`、`docs/*-spec.md`、`agents/` 属保护路径，合入前需人工 review（AGENTS.md 治理门禁）。
