// registry_windows.go 直接用 advapi32，避免仅为三个注册表操作新增依赖。
package driveicon

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

const (
	hkeyLocalMachine = uintptr(0x80000002)

	keyQueryValue       = 0x0001
	keySetValue         = 0x0002
	keyEnumerateSubKeys = 0x0008

	regSZ            = 1
	regExpandSZ      = 2
	errorNoMoreItems = 259
)

var (
	advapi32         = syscall.NewLazyDLL("advapi32.dll")
	shell32          = syscall.NewLazyDLL("shell32.dll")
	regCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	regOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	regSetValueExW   = advapi32.NewProc("RegSetValueExW")
	regQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	regDeleteKeyW    = advapi32.NewProc("RegDeleteKeyW")
	regEnumKeyExW    = advapi32.NewProc("RegEnumKeyExW")
	regCloseKey      = advapi32.NewProc("RegCloseKey")
	shChangeNotify   = shell32.NewProc("SHChangeNotify")
)

// HKLMStore 是 Store 的真实 HKLM 实现。所有写/删权限错误原样返回，
// 由 Manager 决定挂载跳过或 cleanup 非零退出。
type HKLMStore struct{}

func (HKLMStore) Set(letter, value string) error {
	path := driveIconsPath + `\` + letter + `\DefaultIcon`
	key, err := createKey(path, keySetValue)
	if err != nil {
		return err
	}
	defer regCloseKey.Call(key)
	utf16, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	r1, _, _ := regSetValueExW.Call(key, 0, 0, regSZ,
		uintptr(unsafe.Pointer(&utf16[0])), uintptr(len(utf16)*2))
	return statusErr(r1)
}

func (HKLMStore) Get(letter string) (string, error) {
	return getDefault(driveIconsPath + `\` + letter + `\DefaultIcon`)
}

func (HKLMStore) Delete(letter string) error {
	path, err := syscall.UTF16PtrFromString(driveIconsPath + `\` + letter + `\DefaultIcon`)
	if err != nil {
		return err
	}
	r1, _, _ := regDeleteKeyW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(path)))
	return statusErr(r1)
}

func (HKLMStore) List() (map[string]string, error) {
	key, err := openKey(driveIconsPath, keyEnumerateSubKeys|keyQueryValue)
	if err != nil {
		return nil, err
	}
	defer regCloseKey.Call(key)
	out := map[string]string{}
	for index := uintptr(0); ; index++ {
		var name [256]uint16
		nameLen := uint32(len(name))
		r1, _, _ := regEnumKeyExW.Call(key, index, uintptr(unsafe.Pointer(&name[0])),
			uintptr(unsafe.Pointer(&nameLen)), 0, 0, 0, 0)
		if r1 == errorNoMoreItems {
			break
		}
		if r1 != 0 {
			return nil, syscall.Errno(r1)
		}
		letter := syscall.UTF16ToString(name[:nameLen])
		if len(letter) != 1 || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz", rune(letter[0])|0x20) {
			continue
		}
		value, err := getDefault(driveIconsPath + `\` + letter + `\DefaultIcon`)
		if err == ErrNotExist {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[strings.ToUpper(letter)] = value
	}
	return out, nil
}

func createKey(path string, access uintptr) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var key uintptr
	r1, _, _ := regCreateKeyExW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(p)), 0, 0, 0,
		access, 0, uintptr(unsafe.Pointer(&key)), 0)
	if r1 != 0 {
		return 0, syscall.Errno(r1)
	}
	return key, nil
}

func openKey(path string, access uintptr) (uintptr, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var key uintptr
	r1, _, _ := regOpenKeyExW.Call(hkeyLocalMachine, uintptr(unsafe.Pointer(p)), 0,
		access, uintptr(unsafe.Pointer(&key)))
	if r1 != 0 {
		return 0, statusErr(r1)
	}
	return key, nil
}

func getDefault(path string) (string, error) {
	key, err := openKey(path, keyQueryValue)
	if err != nil {
		return "", err
	}
	defer regCloseKey.Call(key)
	var typ, size uint32
	r1, _, _ := regQueryValueExW.Call(key, 0, 0, uintptr(unsafe.Pointer(&typ)), 0,
		uintptr(unsafe.Pointer(&size)))
	if r1 != 0 {
		return "", statusErr(r1)
	}
	if typ != regSZ && typ != regExpandSZ {
		return "", fmt.Errorf("DriveIcons default value has unexpected type %d", typ)
	}
	if size == 0 {
		return "", nil
	}
	buf := make([]byte, int(size)+2)
	r1, _, _ = regQueryValueExW.Call(key, 0, 0, uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 != 0 {
		return "", statusErr(r1)
	}
	u16 := unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), int(size)/2)
	return syscall.UTF16ToString(u16), nil
}

func statusErr(status uintptr) error {
	switch status {
	case 0:
		return nil
	case 2: // ERROR_FILE_NOT_FOUND
		return ErrNotExist
	default:
		return syscall.Errno(status)
	}
}

func notifyShell() {
	// SHCNE_ASSOCCHANGED (0x08000000) + SHCNE_UPDATEIMAGE (0x00008000),
	// flags=SHCNF_IDLIST(0)，无路径参数。SHChangeNotify 返回 void。
	shChangeNotify.Call(0x08000000, 0, 0, 0)
	shChangeNotify.Call(0x00008000, 0, 0, 0)
}
