// driveicon_test.go HKLM DriveIcons 生命周期单测，全部使用内存 Store，
// 不触碰真实注册表。提权写键→SHGetFileInfo→优雅卸载由主会话独立复验。
package m2_test

import (
	"syscall"
	"testing"

	"phonefs/go/internal/driveicon"
)

type fakeIconStore struct {
	values    map[string]string
	setErr    error
	getErr    error
	deleteErr error
	listErr   error
	deletes   []string
}

func (s *fakeIconStore) Set(letter, value string) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.values[letter] = value
	return nil
}

func (s *fakeIconStore) Get(letter string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[letter]
	if !ok {
		return "", driveicon.ErrNotExist
	}
	return v, nil
}

func (s *fakeIconStore) Delete(letter string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if _, ok := s.values[letter]; !ok {
		return driveicon.ErrNotExist
	}
	delete(s.values, letter)
	s.deletes = append(s.deletes, letter)
	return nil
}

func (s *fakeIconStore) List() (map[string]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out, nil
}

func TestDriveIconAccessDeniedSkipsWithoutFailure(t *testing.T) {
	store := &fakeIconStore{values: map[string]string{}, setErr: syscall.Errno(5)}
	notified := 0
	m := driveicon.NewManager(store, testLogger{t}, func() { notified++ })
	if ok := m.Install("P:", `C:\PhoneFS\assets\icons\drive_phone.ico`); ok {
		t.Fatal("Install reported success despite ERROR_ACCESS_DENIED")
	}
	if len(store.values) != 0 || notified != 0 {
		t.Fatalf("denied install wrote state: values=%v notify=%d", store.values, notified)
	}
	// 未记录所有权，卸载是 no-op；调用方无需让挂载失败。
	if err := m.Uninstall("P:"); err != nil {
		t.Fatalf("Uninstall after denied install: %v", err)
	}
}

func TestDriveIconUninstallDoesNotDeleteChangedValue(t *testing.T) {
	store := &fakeIconStore{values: map[string]string{}}
	notified := 0
	m := driveicon.NewManager(store, testLogger{t}, func() { notified++ })
	icon := `C:\PhoneFS\assets\icons\drive_phone.ico`
	if ok := m.Install("P:", icon); !ok {
		t.Fatal("Install failed on writable mock")
	}
	if got := store.values["P"]; got != icon+",0" {
		t.Fatalf("installed value=%q", got)
	}
	// 模拟用户/其他进程在本进程写入后改成自己的图标。
	store.values["P"] = `C:\Users\someone\custom.ico,0`
	if err := m.Uninstall("P:"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got := store.values["P"]; got != `C:\Users\someone\custom.ico,0` {
		t.Fatalf("changed value was deleted/overwritten: %q", got)
	}
	if len(store.deletes) != 0 || notified != 1 { // 仅 Install 通知一次
		t.Fatalf("unexpected delete/notify: deletes=%v notify=%d", store.deletes, notified)
	}
}

func TestDriveIconUninstallAndCleanupResiduals(t *testing.T) {
	store := &fakeIconStore{values: map[string]string{}}
	notified := 0
	m := driveicon.NewManager(store, testLogger{t}, func() { notified++ })
	icon := `C:\PhoneFS\assets\icons\drive_phone.ico`
	if ok := m.Install("Q:", icon); !ok {
		t.Fatal("Install failed")
	}
	if err := m.Uninstall("Q:"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, exists := store.values["Q"]; exists || len(store.deletes) != 1 || notified != 2 {
		t.Fatalf("lifecycle mismatch values=%v deletes=%v notify=%d", store.values, store.deletes, notified)
	}

	// kill 残留清理按 assets/icons 后缀匹配，与二进制/安装目录无关。
	store.values["P"] = `C:\PhoneFS\assets\icons\drive_tablet.ico,0`
	store.values["Q"] = `C:\other\assets\icons\drive_phone.ico,0`
	store.values["R"] = `C:\PhoneFS\assets\icons\readme.txt,0`
	n, err := m.Cleanup()
	if err != nil || n != 2 {
		t.Fatalf("Cleanup n=%d err=%v", n, err)
	}
	if _, exists := store.values["P"]; exists {
		t.Fatal("owned residual not cleaned")
	}
	if _, exists := store.values["Q"]; exists {
		t.Fatal("matching residual in other install not cleaned")
	}
	if store.values["R"] == "" {
		t.Fatalf("non-icon value cleaned: %v", store.values)
	}
}

func TestDriveIconCleanupAccessDenied(t *testing.T) {
	store := &fakeIconStore{
		values:    map[string]string{"P": `C:\PhoneFS\assets\icons\drive_phone.ico,0`},
		deleteErr: syscall.Errno(5),
	}
	m := driveicon.NewManager(store, testLogger{t}, func() {})
	_, err := m.Cleanup()
	if err == nil || !driveicon.IsAccessDenied(err) {
		t.Fatalf("Cleanup err=%v want ACCESS_DENIED (main must exit nonzero)", err)
	}
}
