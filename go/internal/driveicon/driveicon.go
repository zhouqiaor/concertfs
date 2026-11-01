// Package driveicon 管理 HKLM DriveIcons\<盘符>\DefaultIcon 的生命周期。
//
// 仅当前进程成功写入的盘符会在优雅卸载时清理，且清理前再次比较值，
// 不删除其他进程/用户后来改成的图标。kill -9 可能留下静态 ico 路径；
// 下次同盘符挂载会覆盖，也可由管理员运行 -cleanup-icons 清理 assets/icons 残留。
// 本包不写 HKCU，不使用 autorun.inf 作为生效依据（WinFsp 卷上 Shell 忽略它）。
package driveicon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

const driveIconsPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\DriveIcons`

// ErrNotExist 表示盘符 DefaultIcon 键或默认值不存在。
var ErrNotExist = errors.New("driveicon: not exist")

// Logger 与主程序 levelLogger 对齐。
type Logger interface {
	Infof(format string, a ...any)
	Warnf(format string, a ...any)
	Errorf(format string, a ...any)
}

// Store 抽象注册表，单测用内存 mock，生产实现为 HKLMStore。
type Store interface {
	Set(letter, value string) error
	Get(letter string) (string, error)
	Delete(letter string) error
	List() (map[string]string, error)
}

// Manager 记录本进程写过的盘符和值；Uninstall 只清理这些键。
type Manager struct {
	store  Store
	log    Logger
	notify func()

	mu    sync.Mutex
	owned map[string]string
}

// NewManager 构造生命周期管理器。store=nil 使用真实 HKLM；notify=nil 使用
// SHChangeNotify。测试传入内存 Store 和计数 notify，不触碰真实注册表。
func NewManager(store Store, log Logger, notify func()) *Manager {
	if store == nil {
		store = HKLMStore{}
	}
	if notify == nil {
		notify = NotifyShell
	}
	return &Manager{store: store, log: log, notify: notify, owned: map[string]string{}}
}

// Install 写入 <ico绝对路径>,0 并通知 Shell。图标是可选能力：权限不足或
// 其他注册表错误只告警并返回 false，绝不让挂载失败，也不记录虚假所有权。
func (m *Manager) Install(drive, iconPath string) bool {
	letter, err := driveLetter(drive)
	if err != nil {
		m.log.Warnf("盘符图标跳过：%v", err)
		return false
	}
	abs, err := filepath.Abs(iconPath)
	if err != nil {
		m.log.Warnf("盘符图标路径无效 %q：%v", iconPath, err)
		return false
	}
	value := abs + ",0"
	if err := m.store.Set(letter, value); err != nil {
		if IsAccessDenied(err) {
			m.log.Warnf("盘符图标需要管理员，跳过：HKLM DriveIcons\\%s (%v)", letter, err)
		} else {
			m.log.Warnf("盘符图标注册表写入失败，跳过：HKLM DriveIcons\\%s (%v)", letter, err)
		}
		return false
	}
	m.mu.Lock()
	m.owned[letter] = value
	m.mu.Unlock()
	m.notify()
	m.log.Infof("已写盘符图标 HKLM DriveIcons\\%s\\DefaultIcon=%q（本进程持有清理权）", letter, value)
	return true
}

// Uninstall 仅当本进程写过且当前值仍完全一致才删除；被别人改动则保留。
// 重复调用安全。通知只在实际删除后发送。
func (m *Manager) Uninstall(drive string) error {
	letter, err := driveLetter(drive)
	if err != nil {
		return err
	}
	m.mu.Lock()
	value, ok := m.owned[letter]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	current, err := m.store.Get(letter)
	if errors.Is(err, ErrNotExist) {
		m.forget(letter)
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取 DriveIcons\\%s 清理前校验：%w", letter, err)
	}
	if current != value {
		m.forget(letter)
		m.log.Infof("盘符图标值已被其他方修改，卸载不删除：DriveIcons\\%s current=%q ours=%q", letter, current, value)
		return nil
	}
	if err := m.store.Delete(letter); err != nil && !errors.Is(err, ErrNotExist) {
		return fmt.Errorf("删除 DriveIcons\\%s：%w", letter, err)
	}
	m.forget(letter)
	m.notify()
	m.log.Infof("已清理本进程盘符图标 DriveIcons\\%s", letter)
	return nil
}

func (m *Manager) forget(letter string) {
	m.mu.Lock()
	delete(m.owned, letter)
	m.mu.Unlock()
}

// Cleanup 删除默认值指向任意 assets/icons 直属 drive_*.ico 的所有盘符键。
// 按约定资产后缀匹配，与当前二进制输出目录无关；不匹配用户显式 -icon 的
// 其他路径。任何删除失败都继续处理其余盘符，最后返回聚合错误（调用方必须非零退出）。
func (m *Manager) Cleanup() (int, error) {
	values, err := m.store.List()
	if errors.Is(err, ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("枚举 DriveIcons：%w", err)
	}
	deleted := 0
	var errs []error
	for letter, value := range values {
		if !pointsToAssetsIcon(value) {
			continue
		}
		if err := m.store.Delete(letter); err != nil && !errors.Is(err, ErrNotExist) {
			errs = append(errs, fmt.Errorf("删除 DriveIcons\\%s：%w", letter, err))
			continue
		}
		deleted++
		m.log.Infof("已清理残留盘符图标 DriveIcons\\%s=%q", letter, value)
	}
	if deleted > 0 {
		m.notify()
	}
	return deleted, errors.Join(errs...)
}

func pointsToAssetsIcon(value string) bool {
	v := strings.TrimSpace(value)
	// 注册表格式为 path,index；只剥离纯数字索引，保留路径里的逗号。
	if i := strings.LastIndex(v, ","); i >= 0 {
		if _, err := strconv.Atoi(v[i+1:]); err == nil {
			v = v[:i]
		}
	}
	if !filepath.IsAbs(v) {
		return false
	}
	dir := strings.ToLower(filepath.ToSlash(filepath.Dir(filepath.Clean(v))))
	base := strings.ToLower(filepath.Base(v))
	return strings.HasSuffix(dir, "/assets/icons") && strings.HasPrefix(base, "drive_") && filepath.Ext(base) == ".ico"
}

func driveLetter(drive string) (string, error) {
	s := strings.TrimSpace(drive)
	s = strings.TrimSuffix(s, `\`)
	s = strings.TrimSuffix(s, ":")
	if len(s) != 1 || (s[0] < 'A' || s[0] > 'Z') && (s[0] < 'a' || s[0] > 'z') {
		return "", fmt.Errorf("无效盘符 %q", drive)
	}
	return strings.ToUpper(s), nil
}

// IsAccessDenied 识别 ERROR_ACCESS_DENIED(5)，供 -cleanup-icons 给明确管理员提示。
func IsAccessDenied(err error) bool {
	return errors.Is(err, syscall.Errno(5))
}

// NotifyShell 刷新图标关联。常量以 ShlObj.h 为准：
// SHCNE_ASSOCCHANGED=0x08000000，SHCNE_UPDATEIMAGE=0x00008000。
// 主会话 PoC 实测 UPDATEIMAGE 生效；两个都发以覆盖图标缓存与文件关联视图。
func NotifyShell() {
	notifyShell()
}
