# PhoneFS M3 delta/replace 真机黑盒报告

日期：2026-11-01  
设备：HUAWEI TEQU-S2C，`ro.serialno=HOMEVISION202111`，adb `192.168.43.1:5555`  
服务：`https://192.168.43.1:18443`  
隔离根：`/storage/emulated/0/PhoneFS-m3-protocol`

## 结果

**12 PASS / 0 FAIL / 1 ENV_LIMIT**。最终复跑 `run_id=1793535440988933700`；环境受限项不计为 PASS。

| 实测项 | 结果 |
|---|---|
| delta 非 final 不提交；final 仅覆盖两个指定区间，其余字节不变 | PASS |
| delta 扩展保留原 44B prefix，扩展至 71B | PASS |
| 错误 base 返回 412 `PRECONDITION_FAILED`，原目标不变 | PASS |
| delta 目标缺失返回 412，目标仍不存在 | PASS |
| 会话建立后并发整写，final 返回 412 并保留新目标 | PASS |
| final 重传返回原结果，内容不变 | PASS |
| 同一 UploadId 的 legacy→delta 与 delta→legacy 混用均返回 400 `INVALID_ARGUMENT` | PASS |
| PATCH `replace=false` 对已存在目标返回 409，源与目标均不变 | PASS |
| PATCH `replace=true` 后目标为源内容、源 404、目录仅有唯一目标项 | PASS |
| case-only rename | ENV_LIMIT，见下 |
| 递归清理及清理后 404 | PASS |

## TEQU case-only 环境限制

`CaseOnly.bin` 改为 `caseonly.bin` 时，PATCH 返回 200 且新大小写路径可读到原内容，但目录枚举仍保留 `CaseOnly.bin`。因此 TEQU ROM 未真正落盘仅大小写改名；本项明确记为 **ENV_LIMIT 而非 PASS**。未通过故障注入或修改生产代码规避。

## 执行与证据

- 测试：`protocol_test.py`，Python stdlib only，使用 `python -I`。
- 每次运行前校验设备序列号；token 仅从 adb 读入进程环境，未写入证据。
- stdout/stderr：`tequ-m3-protocol.txt`。
- 结构化结果：`protocol-results.json`。
- 测试结束后递归删除隔离根；另经 adb 核验路径不存在。
- 协议矩阵本身为小文件测试；大文件 GET 单独在「大文件流式 GET（>2GiB）内存准入」一节记录（含引擎修复与真机复验）。协议矩阵期间未停止服务、卸载或修改 APK，未改生产代码。

本轮 delta/replace 未发现生产协议 bug。

## 大文件流式 GET（>2GiB）内存准入

M3 硬化把「大文件 GET 不得随文件大小无界增长内存」列为准入门槛。Netty 环回引擎未通过：
2,147,495,993B 文件在交付约 58–65MiB 后 RSS 涨到 4.74–7.29GiB 被厂商 SWAP 杀。纯环回
明文口 A/B（绕过 TLS front）同样爆炸，排除 Conscrypt/TLS 为主因；`respondOutputStream`
换 `respondBytesWriter` + `writeFully/flush` 仍失败（RSS 7.29GiB），确认根因在 Netty
应用/响应路径。

**修复**：把环回 HTTP 引擎从 Netty 换成 CIO。W2 当时选 Netty 仅因 CIO 3.6.1 不能直接服务
HTTPS；现在 TLS 由 `LoopbackTlsFront`（Conscrypt SSLServerSocket → 127.0.0.1 明文）承担，
CIO 只需环回明文，约束消失。同版本钉 3.6.1，TLS front、端口、契约均不变。

**真机复验（同一 2,147,495,993B 文件，sha256=b27908f1…54d4）**：

| 路径 | 结果 | 速率 | RSS 采样 / HWM |
|---|---|---|---|
| 纯环回明文（adb forward） | 全量 200，sha 匹配 | 20.6 MiB/s | ~160–167MB / 184MB |
| HTTPS（LoopbackTlsFront） | 全量 200，sha 匹配 | 20.9 MiB/s | ~159–170MB / 184MB |

整个传输过程 VmRSS 恒定在 ~160–170MB，VmHWM 184MB，未再随流量增长，进程存活。

**回归矩阵（CIO 构建，真机）**：W2 只读 41/41、M2 写 20/20、M3 delta/replace 12/12
（+1 ENV_LIMIT）、rw 桥 15/15、ro 桥 16/16、core JVM 23/23。设备序列号每次复验。

**终轮复验（2026-11-01 22:15–22:22，含 P0-1 修复后的最终构建）**：同一 2.1GB 文件重新生成后
HTTPS 全量 GET 2,147,495,993B、PC 侧 sha256 与设备侧 `sha256sum` 一致
（818e2054…81ab）、84s/24.4 MiB/s、VmRSS 162–178MiB、VmHWM 188MiB；keep-alive 复用已验证
（第二请求 `num_connects=0`）。完整记录见 `cio-largeget-evidence.txt`。

## P0-1 修复：写句柄周期内 Utimens 破坏 delta 基线（数据丢失）

Colossus 首轮抓到的 P0：Windows「覆盖保存」在同一句柄周期内 Open(TRUNC)→Write→SetFileTime。
桥把 Utimens 直接 PATCH meta 到服务端，服务端 ETag＝`<size>-<modified_ms>` 随之改变，而该写句柄
打开时捕获的 base ETag 已失效，Flush 的 delta final 被 412 拒绝——**新内容丢失**。

修复（`bridgefs_write.go` / `bridgefs.go`）：

- 该节点仍有打开写句柄时，Utimens **不再 PATCH**，mtime 记到各写句柄，由上传提交的
  `X-Set-Mtime` 一并生效；节点级 `pendingMtime` 让 Getattr 立即回显，但 `baseETag`/服务端比较
  仍用 `mod`（服务端真值），不污染 delta 基线。
- `Release` 对「只有属性写句柄、无脏数据」的情况兜底 PATCH mtime；同节点仍有其它写句柄时延后，
  最后一个句柄离开才提交（`SetLastWriteTimeUtc` 会另开短命写句柄，正是此路径）。
- 会话绑定 total 自愈：文件在失败会话后继续增长时换新 uploadID，避免同 id 异 total 被 400 卡死。
- 写加载遇服务端 416 不再当成功（否则留下未加载的稀疏零洞）。

真机验证（最终构建）：`p0-scenario`（同句柄周期覆盖）CONTENT=new、MTIME 精确回读；
「移动后 SetLastWriteTimeUtc」GET==SET；rw 桥 15/15 含 utimens-mtime。

测试侧：fake server 补 `modified_ms`/`X-Set-Mtime` 语义使 P0 可复现；新增
`TestUtimensDuringOpenWriteKeepsDeltaBaseline`（无修复时 Flush 返回 -16，修复后绿）。
ro 套件加只读卷预检：卷可写即拒绝运行，避免误写/误删设备 fixture。
