// bridgefs_icon.go M2 盘符图标：根目录只读虚拟 autorun.inf。
//
// Explorer 对固定盘读取 [autorun] Icon=<绝对路径>；文件内容完全在内存合成，
// 不落手机、不写注册表，卸载即消失。无有效图标时不呈现该文件，保持 M1 根布局。
package httpfs

import "github.com/winfsp/cgofuse/fuse"

const (
	autorunPath      = "/autorun.inf"
	virtualAutorunID = "virtual:autorun.inf"
)

// SetAutorunIcon 启用根目录虚拟 autorun.inf。iconPath 必须是已存在的绝对
// 路径（由 cmd 层校验）；本函数只保存内容，不触碰注册表或真实磁盘。
// 挂载前调用；重复调用替换内容并失效根目录缓存。
func (b *BridgeFS) SetAutorunIcon(iconPath string) {
	data := []byte("[autorun]\r\nIcon=" + iconPath + "\r\n")
	b.mu.Lock()
	defer b.mu.Unlock()
	n := &node{
		id: virtualAutorunID, name: "autorun.inf", size: int64(len(data)),
		mod: b.mountTime, writable: false, ino: 2,
	}
	b.autorun = n
	b.autorunData = data
	b.byPath[autorunPath] = n
	delete(b.dirs, "")
	b.log.Infof("virtual autorun.inf enabled: Icon=%s (%d bytes)", iconPath, len(data))
}

// appendVirtualRootLocked 把 autorun.inf 加到根列表末尾（8 个目录根之后）。
// 调用方持 b.mu。
func (b *BridgeFS) appendVirtualRootLocked(dirPath string, nodes []*node) []*node {
	if dirPath == "" && b.autorun != nil {
		return append(nodes, b.autorun)
	}
	return nodes
}

func isVirtualAutorun(n *node) bool {
	return n != nil && n.id == virtualAutorunID
}

// readVirtualAutorun 内存读，支持任意偏移/短读与 EOF，不走服务端 Range。
func (b *BridgeFS) readVirtualAutorun(buff []byte, ofst int64) int {
	if ofst < 0 {
		return -fuse.EINVAL
	}
	if ofst >= int64(len(b.autorunData)) {
		return 0
	}
	return copy(buff, b.autorunData[ofst:])
}
