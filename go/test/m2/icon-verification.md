# M2 盘符图标验证（2026-11-01）

## 实现范围

- `-icon <绝对 ico 路径>` 显式指定图标；否则按 `pair/info device.form_factor` 的 `phone/tablet/pc` 选择可执行文件相对路径 `assets/icons/drive_<form_factor>.ico`。
- 缺字段、`other` 或默认图标文件缺失时不生成 `autorun.inf`。
- 启用后根目录呈现只读虚拟 `autorun.inf`，内容为 `[autorun]\r\nIcon=<绝对路径>\r\n`；支持普通/偏移/EOF 读，rw 挂载下写、删、改名、截断仍拒绝。
- 全程未修改注册表。

## 真实 Shell 通道结果：未生效

真实 TEQU-S2C 服务端、WinFsp 2.1、`P:` 固定 FUSE 卷、`-ro -icon .../drive_phone.ico`：

- 通过盘符读取 `P:\autorun.inf`，内容与预期逐字节一致（证据 `runs/icon-shell-phone.json/.txt`）。
- `SHGetFileInfo(P:\, SHGFI_ICONLOCATION)` 返回 **`C:\WINDOWS\system32\imageres.dll`, index `-32`**，不是指定的 phone ico。
- `SHGetFileInfo(P:\, SHGFI_ICON)` 能提取 32×32 图标句柄，但对应上述系统默认盘符图标；提取图保存为 `runs/icon-shell-phone.png`，不能误报为 PhoneFS 图标成功。
- 结论：**本机 Explorer/Shell 忽略 WinFsp 卷的 autorun.inf 图标项**。虚拟文件协议与读路径已验证，盘符图标视觉效果验收失败，证据如实保留。

复跑：挂载后执行 `test/m2/verify-icon-shell.ps1 -Drive P: -ExpectedIcon <ico 绝对路径> -Tag phone`。脚本仅读文件并调用 Shell API，不写注册表。

## 最终定论：HKLM DriveIcons 生效（主会话提权实证）

主会话继续对照验证：HKCU DriveIcons 无效；提权写
`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\DriveIcons\Q\DefaultIcon`
后，`SHGetFileInfo(Q:\)` 的图标位置变为 `drive_phone.ico`，提取句柄也确认是手机图。
证据 `runs/icon-shell-mainhklm.json/.png/.txt`。因此最终方案是 **DriveIcons 生命周期管理**；
autorun.inf 代码保留，但明确标注对本 WinFsp 卷无效。

桥侧实现：

- 图标启用时写 `DriveIcons\<盘符>\DefaultIcon` 默认值为 `<ico绝对路径>,0`，随后发送
  `SHCNE_ASSOCCHANGED=0x08000000` 与 `SHCNE_UPDATEIMAGE=0x00008000`（后者为主会话 PoC 实测生效值）。
- 非管理员 `ERROR_ACCESS_DENIED` 明确告警「盘符图标需要管理员，跳过」，挂载继续，绝不假报写入成功。
- 仅记录本进程成功写入的盘符；优雅卸载时再次读取并逐字比较，值已被其他方改动则不删除。
- kill -9 可能残留静态 ico 路径，无安全面；下次同盘符挂载会覆盖。管理员可运行
  `-cleanup-icons` 清理所有指向 `assets/icons/drive_*.ico` 的残留键；权限不足或任何删除失败均非零退出。
- `test/m2/driveicon_test.go` 用注册表 mock 覆盖无管理员跳过、他方改值后卸载不误删、正常清理与残留清理；
  提权端到端写键→图标生效→优雅卸载键消失由主会话独立复验。
