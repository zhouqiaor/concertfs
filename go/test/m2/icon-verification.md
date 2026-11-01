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

## DriveIcons（仅后备方案，未实施）

若后续必须实现视觉图标，可评估安装器在管理员授权下写
`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\DriveIcons\<盘符>\DefaultIcon`，
并在卸载时清理、处理盘符复用和 Explorer 刷新。本次任务明确禁止改注册表，故未采用、未验证该方案；不把注册表键路径描述为已实现能力。
