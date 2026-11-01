// icon_test.go M2 虚拟 autorun.inf 单测：内存内容、大小写解析、根目录呈现、
// rw 下仍只读；不含真实 Shell 验证（由 verify-icon-shell.ps1 真机/真挂载完成）。
package m2_test

import (
	"context"
	"testing"

	"github.com/winfsp/cgofuse/fuse"
)

func TestAutorunInfVirtualReadAndReadOnly(t *testing.T) {
	f := newFixture(t, false) // rw 挂载也不能改虚拟配置
	const icon = `C:\PhoneFS\assets\icons\drive_phone.ico`
	f.b.SetAutorunIcon(icon)
	want := "[autorun]\r\nIcon=" + icon + "\r\n"

	var st fuse.Stat_t
	if errc := f.b.Getattr("/AUTORUN.INF", &st, ^uint64(0)); errc != 0 {
		t.Fatalf("case-insensitive Getattr errc=%d", errc)
	}
	if st.Size != int64(len(want)) || st.Mode&fuse.S_IFREG == 0 {
		t.Fatalf("autorun stat size=%d mode=%#o", st.Size, st.Mode)
	}
	errc, fh := f.b.Open("/autorun.inf", fuse.O_RDONLY)
	if errc != 0 {
		t.Fatalf("Open errc=%d", errc)
	}
	got := make([]byte, len(want))
	if n := f.b.Read("/autorun.inf", got[:5], 0, fh); n != 5 {
		t.Fatalf("Read head n=%d", n)
	}
	if n := f.b.Read("/autorun.inf", got[5:], 5, fh); n != len(want)-5 {
		t.Fatalf("Read rest n=%d", n)
	}
	if string(got) != want {
		t.Fatalf("autorun content=%q want=%q", got, want)
	}
	if n := f.b.Read("/autorun.inf", make([]byte, 8), int64(len(want)), fh); n != 0 {
		t.Fatalf("EOF read n=%d want 0", n)
	}
	if errc := f.b.Release("/autorun.inf", fh); errc != 0 {
		t.Fatalf("Release errc=%d", errc)
	}

	// 根目录除 8 个服务端根外呈现第 9 项 autorun.inf。
	errc, dh := f.b.Opendir("/")
	if errc != 0 {
		t.Fatalf("Opendir errc=%d", errc)
	}
	names := map[string]bool{}
	if errc := f.b.Readdir("/", func(name string, st *fuse.Stat_t, ofst int64) bool {
		names[name] = true
		return true
	}, 0, dh); errc != 0 {
		t.Fatalf("Readdir errc=%d", errc)
	}
	if !names["autorun.inf"] {
		t.Fatalf("root listing lacks autorun.inf: %v", names)
	}

	if errc, _ := f.b.Open("/autorun.inf", fuse.O_WRONLY); errc != -fuse.EROFS {
		t.Fatalf("write Open errc=%d want EROFS", errc)
	}
	if errc := f.b.Unlink("/autorun.inf"); errc != -fuse.EROFS {
		t.Fatalf("Unlink errc=%d want EROFS", errc)
	}
	if errc := f.b.Rename("/autorun.inf", dir+"/autorun.inf"); errc != -fuse.EROFS {
		t.Fatalf("Rename errc=%d want EROFS", errc)
	}
	if errc := f.b.Truncate("/autorun.inf", 0, ^uint64(0)); errc != -fuse.EROFS {
		t.Fatalf("Truncate errc=%d want EROFS", errc)
	}
	if errc := f.b.Utimens("/autorun.inf", nil); errc != -fuse.EROFS {
		t.Fatalf("Utimens errc=%d want EROFS", errc)
	}
	if errc := f.b.Access("/autorun.inf", fuse.W_OK|fuse.DELETE_OK); errc != -fuse.EACCES {
		t.Fatalf("Access write/delete errc=%d want EACCES", errc)
	}
}

func TestAutorunInfAbsentWithoutIcon(t *testing.T) {
	f := newFixture(t, false)
	var st fuse.Stat_t
	if errc := f.b.Getattr("/autorun.inf", &st, ^uint64(0)); errc != -fuse.ENOENT {
		t.Fatalf("autorun without icon errc=%d want ENOENT", errc)
	}
}

func TestPairInfoFormFactor(t *testing.T) {
	f := newFixture(t, false)
	pi, err := f.cl.PairInfo(context.Background())
	if err != nil {
		t.Fatalf("PairInfo: %v", err)
	}
	if pi.DeviceName != "FAKE" || pi.DeviceFormFactor != "phone" {
		t.Fatalf("PairInfo=%+v", pi)
	}
}
