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
- 仅执行小文件协议测试，未运行大文件 GET/吞吐，未停止服务、卸载或修改 APK，未改生产代码。

本轮 delta/replace 未发现生产协议 bug。
