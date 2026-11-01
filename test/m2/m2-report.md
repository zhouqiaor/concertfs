# PhoneFS M2 写操作真机验收报告

日期：2026-11-01
设备：HUAWEI TEQU-S2C（Android 12 / API 31，adb `192.168.43.1:5555`）
服务：`https://192.168.43.1:18443`，TLS 1.3
最终 APK SHA-256：`9a037ea78b7ddfed4c64f15d22303eaa32bbfc62cec9919d80d77e41be55642f`

## 交付范围

- Android：`POST /v1/fs/create`、整写/分块 `PUT /v1/fs/content`、`PATCH /v1/fs/meta`、幂等 `DELETE /v1/fs`；`X-Set-Mtime` 与 MediaStore 回映。
- Go/cgofuse：Create/Write/Truncate/Flush/Fsync/Release/Unlink/Rename/Mkdir/Rmdir/Utimens；默认 rw，`-ro` 保留 M1 只读行为。
- 整合 Go 桥写句柄使用 PC 侧整文件内存映像（`f.data []byte`）承接随机写，Flush/Fsync/Release 以 1 MiB `Content-Range` 分块上传；写后递增节点版本，其他已打开读句柄会废弃旧 Range 块。
- 代码改动仅在 `C:/Code/Wideawake-wt-phonefs-m2` / `feat/m2-write`；rw 复验按 coordinator 要求调用整合树 Go 桥与 harness。未合 main、未 push。

## 契约决策与边界

- 上传会话为进程内 24 h TTL；服务停止清理未完成临时文件，不承诺跨进程续传。
- 绝对末块先到但区间未覆盖完整时返回 `409 UPLOAD_INCOMPLETE`；后续任意块使区间完整即提交，不死等逻辑末块。
- 完成态同 Upload-Id 重传只消费请求体；若目标已删除/重建且大小不符，旧完成态失效并重新上传。
- M2 目录 rename/move/meta 返回 501；非零长度 truncate 按 SPEC P2-12 返回 ENOSYS，截零正常支持。
- TEQU-S2C `/storage/emulated/0` 的 mtime 实测为秒级精度，证据不伪报毫秒等值。

## 最终验收

| 验收 | 结果 | 证据 |
|---|---:|---|
| Android 写协议黑盒 | 20/20 | `tequ-m2-protocol-colossus.txt`、`protocol-results.json` |
| 整合树 Go rw 桥真机矩阵 | 15/15 | `tequ-m2-bridge-colossus.txt`、`bridge-results-colossus.json` |
| mtime false/throw 故障注入 JVM 回归 | 2/2 | `unit-upload-store.txt`、`unit-upload-store-results.xml` |
| Go `encoding/json` 数值水位与续传缝测试 | 通过 | `tequ-go-details-resume.txt` |
| `-ro` M1 回归 | 16/16 | `tequ-m2-ro-16.txt`、`../../go/test/m1/runs/m1-results-m2-ro.json` |
| W2 只读功能矩阵（含指纹） | 41/41 | `tequ-m2-readonly-functional-colossus.txt` |
| MediaStore 创建/rename/delete 回映 | 通过 | `tequ-m2-mediastore.txt` |
| Go vet/test/build | 通过 | `phonefs/go`（nocgo） |

rw 矩阵使用整合树 `C:/Code/Wideawake-wt-feat-integrate` 的 Go 桥与 harness 复跑，覆盖多分块写读 SHA、随机非对齐覆盖与扩展、已打开句柄缓存失效、截零/非零截断拒绝、rename/move/mtime、EEXIST/ENOTEMPTY、Unlink/Rmdir、Shell.Application.CopyHere 和隔离树清理。Explorer 同源 CopyHere 文件为 3,200,123 字节，FUSE 读回与 adb 手机直读 SHA-256 均为 `4221BFA341FBA227EAB7ADF05CEDD3783AF4CE8633F860E26AD921411175B1D9`。整合桥二进制 SHA-256 见 `integrate-bridge-sha256.txt`。

MediaStore 数据库级证据：创建后 `_id=456`、`_size=68`；rename 后旧路径无记录、同 `_id=456` 指向新路径；delete 后新路径也无记录。

## 验收发现与修复

rw 第一轮曾触发服务端 `IllegalMonitorStateException`：`ReentrantLock` 在协程挂起后跨线程恢复，finally unlock 非持有线程。已改为 `kotlinx.coroutines.sync.Mutex`，随后协议 20/20、rw 15/15、ro 16/16、只读 41/41 全部通过。修复提交：`5ad0a20 fix(source): 分块上传串行锁改用协程 Mutex`。

Colossus P1 闭环：

- P1-1：原子 move 成功即认定内容完成；mtime 与 MediaStore 回映失败只告警，不再把同 Upload-Id 留在未完成态，消除稀疏 temp 重试覆盖完整目标的窗口。JVM 注入 `setLastModified=false` 和抛异常两条回归均通过。
- P1-2：`PhoneFsException.details` 在 JSON 边界按 Number/Boolean/String 转换，`UPLOAD_INCOMPLETE.details.received` 现发 JSON number。真实 Go `encoding/json` 解码为 `float64(100)`，按该水位补传 100..899 后完整读回 SHA。
- P1-3：报告已按整合实现订正为 PC 整文件内存映像，并登记大文件内存、rename/临时文件/会话配额限制。

## 未覆盖/挂账

- 24 h TTL 已实现过期清理，但未进行真实 24 h 等待测试。
- DELETE 的 `409 PARTIAL` 分支已实现；本轮未做需要制造中途 I/O 故障的物理故障注入，NOT_EMPTY、幂等删除和递归清理已实测。
- 整合 Go 桥大文件写时 PC 侧内存占用约等于文件大小，加载原文与写映像叠加时峰值更高；M3 必须改落盘暂存/增量脏块。每个脏写周期 Flush 仍会重传完整文件，这是 M2 正确性基线，不是性能终态。
- 同目录仅大小写改名在大小写不敏感文件系统上可能被服务端误判为已存在并返回 409；M3 评估同 inode 豁免与跨 ROM conformance。
- Go 桥 rename 替换当前为先 DELETE 目标再 PATCH，PATCH 失败时目标已丢失、源保留，不是 POSIX 原子替换；M3 需服务端 replace 语义或事务补偿。
- 上传临时文件在硬崩溃后可能残留 `.phonefs-*.tmp`，且 sessions/ranges 当前无数量上限；M3 增加启动清扫、会话/区间配额与背压。

## 相关提交

- `70882fd feat(source): pair/info 增加 device.form_factor`（独立追加任务，TEQU-S2C=`other`，`device.type=android`）
- `6eeb835 feat(source): M2 写端点与分块幂等上传`
- `5ad0a20 fix(source): 分块上传串行锁改用协程 Mutex`
- `d465f6b feat(bridge): M2 默认 rw 写桥与真机验收`
- Colossus P1 修复、跨语言/故障注入回归与报告订正：本报告所在提交

所有测试写入均限定于 `PhoneFS-m2-protocol`、`PhoneFS-m2-bridge`、`PhoneFS-m2-media`、`PhoneFS-m2-go-details` 等隔离根，最终已清理；证据不包含 Bearer token。
