# phonefs/go — W1 闸门 spike 复现说明

M0-W1（cgofuse + WinFsp nocgo 挂载实测）的一次性验证代码。产品实现阶段（M1）本目录
按 SPEC §12.1 长为 16 个 `internal/` 包 + 3 个 `cmd/`；**W1 不落 internal 产品结构**，
仅 `cmd/w1spike/`（内存 FS + 挂载宿主），M1 由 `internal/fshost` + `cmd/phonefs-fs` 取代。

## 前置（一次性，已装版本见闸门报告）

1. Go **windows/amd64** 1.26.x（W1 实测 1.26.2，解压版即可，不动 386 版）：
   `C:\Programs\go-amd64\bin\go.exe`，`go env -w GOPROXY=https://goproxy.cn,direct`
2. WinFsp **2.1.25156** MSI（管理员一次，ADDLOCAL=ALL）；仅运行期需要 `winfsp-x64.dll`。
3. 无需 gcc/MSYS2——cgofuse v1.6.0 Windows nocgo 路径运行期 `LoadLibraryW` 加载 DLL。

## 构建（必须 nocgo）

```bash
cd projects/phonefs/go
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  /c/Programs/go-amd64/bin/go.exe build -trimpath -o cmd/w1spike/w1spike.exe ./cmd/w1spike
# 产物 PE32+ x86-64，约 2.9 MiB；go version -m 可见 build CGO_ENABLED=0
```

## 挂载

```bash
# auto=从 P: 起找第一个空闲盘符；-trace 记录每个 FUSE 回调
./cmd/w1spike/w1spike.exe -mount auto -trace -log test/w1/runs/manual-trace.log
# 固定盘符： -mount P:
# Ctrl-C / CTRL_BREAK 优雅卸载（Destroy 回调、盘符立即消失）；强杀也无僵尸盘
```

挂载成功标志：日志出现 `The service w1spike has been started.`；
`Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='P:'"` → DriveType=3(fixed)、
FileSystem=FUSE、VolumeName=PhoneFS-W1、Size=1TiB。

## 操作清单与挂卸循环

```powershell
# 29 项脚本化操作（建目录/读写改截/改名移动删除/时间戳属性/只读拒绝/statfs/
# Explorer IFileOperation 复制+覆盖/回收站/中文+emoji+203 字符长名/100MB sha256/
# 千项目录/递归删/大小写不敏感/Shell 浏览）
powershell -NoProfile -ExecutionPolicy Bypass -File ..\..\..\test\w1\run-w1-tests.ps1 -Drive P: -Tag manual
# 连挂连卸 3 轮（CTRL_BREAK 优雅信号 + 无残留校验）
powershell -NoProfile -ExecutionPolicy Bypass -File ..\..\..\test\w1\mount-cycles.ps1 -Cycles 3
```

结果在 `test/w1/runs/`：`w1-results-<tag>.{txt,json}`、`mount-cycles.txt`、`cycle*-trace.log`。

## W1 已验证语义（给 M1 internal/fshost 的注意点）

- Create 收到的 flags 恒为 `0x502 = O_RDWR|O_CREAT|O_EXCL`（WinFsp 原生 Create 语义，
  cgofuse v1.6.0 nocgo 常量已与 CRT 对齐，issue #66 常量混用已修）。
- **Setxattr 必须返回 0**：Defender 建文件时可能带 `$KERNEL.PURGE.SEC.FILEHASH` EA，
  返回 ENOSYS 会让 Explorer 原生复制失败（issue #66 的另一半根因）。
- 大小写不敏感：`SetCapCaseInsensitive(true)` + 自实现 EqualFold 解析，仅置位无效（#91/#93）。
- 删除：WinFsp delete-on-cleanup 映射为 POSIX unlink；目录项摘除后打开句柄续命。
- Rename 需支持覆盖已存在普通文件（空目录才可覆盖）。
- 回收站：FUSE(POSIX) 层不走 SetDispositionInfo 回收站协议，Shell 静默不回收（W1 条件项，
  非退出标准）；M1 记风险册，评估 WinFsp 原生 API 绑定（winfsp-go 类）。
- 写吞吐对块大小敏感（#86）：M4 基准要扫块大小；W1 内存盘 100MB 复制 0.06–0.13s。
