# PhoneFS 盘符图标

- `drive_phone.ico`、`drive_tablet.ico`、`drive_pc.ico`：原创几何图形，分别含 256×256、48×48、32×32 三个尺寸。
- 来源：`C:/Code/Wideawake/.scratch/2026-11-01-drive-icon-analysis/`（Professor-X REA 子会话，2026-11-01），由同目录 `make_icons.py` 使用 Pillow 绘制；未使用华为、微软或其他第三方图标资源。
- 运行时可执行文件按自身相对路径查找 `assets/icons/drive_<form_factor>.ico`；发布时必须保持目录结构与路径稳定，避免 Explorer 图标缓存指向旧资源。

## MIT License

Copyright (c) 2026 PhoneFS contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
