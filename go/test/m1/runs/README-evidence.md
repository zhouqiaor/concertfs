# PhoneFS M1 证据链说明（runs/）

本目录是 M1 只读盘符桥的验收证据单一事实源。Colossus 门禁
`review: ocr pass [0P0/3P1/13P2]`（评审全文 `.scratch/2026-11-01-m1-colossus/review.md`），
3 条 P1 已在合入前闭环，P2 1-13 全部挂 M2/W3 账（本文件登记即可，不展开重构）。

## 结果总表

| 标签 | 结果 | 卷标 | 二进制 | 说明 |
|---|---|---|---|---|
| round1 | 14/16 | HUAWEI TEQU-S2C | 早期迭代 | 两处 FAIL 根因见下，均已修复 |
| round2 | 16/16 | HUAWEI TEQU-S2C | 含 SetCapDeleteAccess | 执行会话全绿 |
| mainverify | 16/16 | HUAWEI TEQU-S2C | 调度方自建 | 调度方独立复跑（产物由主会话生成） |
| labelchange | 16/16 | TEQU-S2C | 调度方自建 | device.name 变更后动态卷标对标复跑 |
| mainverify2 | 16/16 | TEQU-S2C | P1 修复后（Q:） | P1-1/P1-2 修复无回归 |

## round1 两处 FAIL 的根因（时间线按实记录）

- **FAIL[08] random/bin-mid-unaligned**：测试脚本自身 bug，非桥缺陷。
  round1 时 `Fetch-Ref bin-mid` 参考窗为 4096B（`5242880-5246975`），而 Case 08
  从 +1 未对齐偏移切 4096B 需要 4097B 源数据，`[Array]::Copy` 报「源数组长度不足」。
  现版 run-m1-tests.ps1 已修为 4097B 参考窗（`5242880-5246976`），round2 起通过。
- **FAIL[13] readonly/delete-denied**：round1 跑的是 **SetCapDeleteAccess 引入前**
  的二进制。WinFsp delete-on-cleanup 语义下 DeleteFile 先报成功，Unlink（ENOSYS）
  在句柄清理时才被调用、错误传不回调用方，且 Readdir 强制刷新让文件「复活」。
  修复：`SetCapDeleteAccess(true)` + `Access(W_OK|DELETE_OK)` 拒绝 + 写操作显式
  EROFS（集成分支 48fba4b；本工作树 538a645）。round2 起删除被即时拒绝。

## 调度方独立复跑登记

- `m1-results-mainverify.json/.txt`：16/16（卷标 HUAWEI TEQU-S2C），主会话独立构建挂载复跑。
- `m1-results-labelchange.json/.txt`：16/16（卷标 TEQU-S2C，device.name 对标后）。
- 主会话独立 soak：14 轮 / 307s 零错误（脚本 `%TEMP%\m1-mainsoak.ps1`，未入库），
  kill -9 后盘符干净消失。入库 soak 以 `m1-soak-soak1.json` 为准：**59 轮 / 301.8s /
  59 readOK / 0 errors / kill 后 0.5s 盘符消失**（数字更强，口径以此为准）。
- 参考字节目录 `ref-mainverify/`、`ref-labelchange/` 等由主会话复跑产生，
  按 .gitignore 不入库（10MiB 级），需要时可重跑脚本再生。

## P1 修复验证（p1fix-verify-note.txt）

- P1-1：合成容量 1<<18 → 1<<28 块，2^28×4096 = 2^40 = 1TiB，注释/日志一致。
  合成路径真机不触发（statfs 始终可用），算术核对，未强行构造故障。
- P1-2：`-label 'X,rw=Y'` → 卷标 `XrwY`（逗号/等号在进 WinFsp fuse_opt_parse
  前剔除）；注入尝试后删除仍被拒绝。mainverify2（Q:，修复后二进制）16/16 无回归。
- 备注：mainverify2 首次尝试（14:03，P:）误跑在调度方旧挂载实例上，已由
  14:04 Q: 自有实例复测取代；调度方 P: 实例全程未动。

## 其余证据文件

- `build-info.txt`：go1.26.2 / CGO_ENABLED=0 / cgofuse v1.6.0 h1（与 go.sum 一致）。
- `auth-preflight.txt`：坏 token 预检 401 快速 FATAL，不留半挂卷。
- `resilience-note.txt`：服务端被杀后目录读明确 EIO、桥不死，FGS 重启后免重挂恢复。
- `graceful-unmount-note.txt`：Ctrl-C 优雅卸载 + 强杀两条路径盘符均干净消失。
- `m1-bridge-*.txt`、`keepalive-log.txt`、`ref-*/`、`*.pid`：运行态产物，gitignore 不入库。

P2 1-13（短块校验/节点驱逐/大小写碰撞/保留名/singleflight/锁粒度/200 回退放大/
命令行 token/卸载期取消/statfs 日志刷屏/Readdir 皮毛/跨平台 build tag/soak 口径）
全部挂 M2/W3，不在 M1 展开。
