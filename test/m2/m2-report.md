# PhoneFS M2 写操作真机验收报告

日期：2026-11-01
设备：HUAWEI TEQU-S2C（Android 12 / API 31，adb `192.168.43.1:5555`）
服务：`https://192.168.43.1:18443`，TLS 1.3
最终 APK SHA-256：`44b7ff29f5bf0c585ba924f3cc01d2a25aad74adb80cc17d314404ba8dd7af66`

## 交付范围

- Android：`POST /v1/fs/create`、整写/分块 `PUT /v1/fs/content`、`PATCH /v1/fs/meta`、幂等 `DELETE /v1/fs`；`X-Set-Mtime` 与 MediaStore 回映。
- Go/cgofuse：Create/Write/Truncate/Flush/Fsync/Release/Unlink/Rename/Mkdir/Rmdir/Utimens；默认 rw，`-ro` 保留 M1 只读行为。
- 写路径使用 PC 本地临时文件承接随机写，Flush/Fsync/Release 以 1 MiB `Content-Range` 分块上传；写后递增节点版本，其他已打开读句柄会废弃旧 Range 块。
- 全程只操作 `C:/Code/Wideawake-wt-phonefs-m2` / `feat/m2-write`，未合 main、未 push。

## 契约决策与边界

- 上传会话为进程内 24 h TTL；服务停止清理未完成临时文件，不承诺跨进程续传。
- 绝对末块先到但区间未覆盖完整时返回 `409 UPLOAD_INCOMPLETE`；后续任意块使区间完整即提交，不死等逻辑末块。
- 完成态同 Upload-Id 重传只消费请求体；若目标已删除/重建且大小不符，旧完成态失效并重新上传。
- M2 目录 rename/move/meta 返回 501；非零长度 truncate 按 SPEC P2-12 返回 ENOSYS，截零正常支持。
- TEQU-S2C `/storage/emulated/0` 的 mtime 实测为秒级精度，证据不伪报毫秒等值。

## 最终验收

| 验收 | 结果 | 证据 |
|---|---:|---|
| Android 写协议黑盒 | 20/20 | `tequ-m2-protocol-final.txt`、`protocol-results.json` |
| Go rw 桥真机矩阵 | 15/15 | `tequ-m2-bridge-round2.txt`、`bridge-results-round2.json` |
| `-ro` M1 回归 | 16/16 | `tequ-m2-ro-16.txt`、`../../go/test/m1/runs/m1-results-m2-ro.json` |
| W2 只读功能矩阵（含指纹） | 41/41 | `tequ-m2-readonly-functional-final.txt` |
| MediaStore 创建/rename/delete 回映 | 通过 | `tequ-m2-mediastore.txt` |
| Go vet/test/build | 通过 | `phonefs/go`（nocgo） |

rw 矩阵覆盖多分块写读 SHA、随机非对齐覆盖与扩展、已打开句柄缓存失效、截零/非零截断拒绝、rename/move/mtime、EEXIST/ENOTEMPTY、Unlink/Rmdir、Shell.Application.CopyHere 和隔离树清理。Explorer 同源 CopyHere 文件为 3,200,123 字节，FUSE 读回与 adb 手机直读 SHA-256 均为 `F89B7C65DC93BB418989269A65F98FA53F4CAF011EDC13308F7A2A2107CDB690`。

MediaStore 数据库级证据：创建后 `_id=456`、`_size=68`；rename 后旧路径无记录、同 `_id=456` 指向新路径；delete 后新路径也无记录。

## 验收发现与修复

rw 第一轮曾触发服务端 `IllegalMonitorStateException`：`ReentrantLock` 在协程挂起后跨线程恢复，finally unlock 非持有线程。已改为 `kotlinx.coroutines.sync.Mutex`，随后协议 20/20、rw 15/15、ro 16/16、只读 41/41 全部通过。修复提交：`5ad0a20 fix(source): 分块上传串行锁改用协程 Mutex`。

## 未覆盖/挂账

- 24 h TTL 已实现过期清理，但未进行真实 24 h 等待测试。
- DELETE 的 `409 PARTIAL` 分支已实现；本轮未做需要制造中途 I/O 故障的物理故障注入，NOT_EMPTY、幂等删除和递归清理已实测。
- 随机写打开会按块落地原文件；每个脏写周期 Flush 会重传完整暂存文件。这是 M2 正确性基线，不是增量脏块性能终态。

## 相关提交

- `70882fd feat(source): pair/info 增加 device.form_factor`（独立追加任务，TEQU-S2C=`other`，`device.type=android`）
- `6eeb835 feat(source): M2 写端点与分块幂等上传`
- `5ad0a20 fix(source): 分块上传串行锁改用协程 Mutex`
- Go rw 写桥、验收脚本与本报告：本报告所在提交

所有测试写入均限定于 `PhoneFS-m2-protocol`、`PhoneFS-m2-bridge`、`PhoneFS-m2-media` 等隔离根，最终已清理；证据不包含 Bearer token。
