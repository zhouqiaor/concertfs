// Command w1spike 是 PhoneFS M0-W1 技术闸门用的内存文件系统挂载 spike。
//
// 它把一个纯内存树通过 cgofuse + WinFsp 挂为 fixed 盘符，用于验证：
// nocgo 纯 Go 构建（零 gcc）、WinFsp 2.1 运行期 ABI、Explorer 原生文件语义。
// 本命令是 M0 一次性验证件，M1 产品实现落在 internal/fshost + cmd/phonefs-fs。
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

type levelLogger struct {
	info  *log.Logger
	trace *log.Logger
}

func newLevelLogger(w io.Writer, trace bool) *levelLogger {
	l := log.New(w, "", log.LstdFlags|log.Lmicroseconds)
	tr := l
	if !trace {
		tr = log.New(io.Discard, "", 0)
	}
	return &levelLogger{info: l, trace: tr}
}

func (l *levelLogger) Fatalf(format string, a ...any) {
	l.info.Printf("FATAL "+format, a...)
	os.Exit(1)
}
func (l *levelLogger) Infof(format string, a ...any)  { l.info.Printf("INFO  "+format, a...) }
func (l *levelLogger) Warnf(format string, a ...any)  { l.info.Printf("WARN  "+format, a...) }
func (l *levelLogger) Errorf(format string, a ...any) { l.info.Printf("ERROR "+format, a...) }
func (l *levelLogger) Tracef(format string, a ...any) { l.trace.Printf("TRACE "+format, a...) }

// freeDriveLetter 从 start 起向上找第一个未占用盘符（内核 GetLogicalDrives 位掩码）。
func freeDriveLetter(start byte) (string, error) {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	proc := k32.NewProc("GetLogicalDrives")
	mask, _, _ := proc.Call()
	for c := start; c <= 'Z'; c++ {
		if mask&(1<<(c-'A')) == 0 {
			return string(c) + ":", nil
		}
	}
	return "", fmt.Errorf("no free drive letter from %c:..Z:", start)
}

func main() {
	mount := flag.String("mount", "auto", "挂载盘符（如 P:）；auto 从 P: 起顺延")
	logPath := flag.String("log", "", "日志文件路径（同时输出到 stderr；空则仅 stderr）")
	label := flag.String("label", "PhoneFS-W1", "卷标（volname 挂载选项，失败时自动去除重试）")
	trace := flag.Bool("trace", false, "记录每个 FUSE 回调")
	capMB := flag.Int64("cap-mb", 1024*1024, "statfs 报告的卷容量（MiB），默认 1 TiB")
	flag.Parse()

	var w io.Writer = os.Stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("open log %q: %v", *logPath, err)
		}
		defer f.Close()
		w = io.MultiWriter(os.Stderr, f)
	}
	lg := newLevelLogger(w, *trace)

	mp := *mount
	if mp == "auto" {
		var err error
		mp, err = freeDriveLetter('P')
		if err != nil {
			lg.Fatalf("%v", err)
		}
	}

	fs := newMemfs(lg, *trace, *capMB*1024*1024)
	host := fuse.NewFileSystemHost(fs)
	host.SetCapCaseInsensitive(true) // Windows 语义；memfs 侧已做大小写不敏感解析
	host.SetCapReaddirPlus(true)     // Readdir 直接带 stat，少一轮 Getattr

	// uid/gid=-1：WinFsp FUSE 层把所有归属映射到当前登录用户；umask=00 不剥权限位。
	opts := []string{"-o", "uid=-1,gid=-1,umask=00"}
	withLabel := *label != ""
	if withLabel {
		opts = append(opts, "-o", "volname="+*label)
	}

	// 优雅卸载：Ctrl-C → host.Unmount()，Mount 随即返回；第二次信号强退。
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt)
	go func() {
		s := <-sigc
		lg.Infof("received signal %v: unmounting %s ...", s, mp)
		if !host.Unmount() {
			lg.Warnf("host.Unmount returned false")
		}
		time.Sleep(3 * time.Second)
		<-sigc
		lg.Errorf("second signal: force exit")
		os.Exit(1)
	}()

	absLog, _ := filepath.Abs(*logPath)
	lg.Infof("w1spike starting: mount=%s label=%q trace=%v cgofuse=v1.6.0 nocgo log=%s",
		mp, *label, *trace, absLog)
	t0 := time.Now()
	ok := host.Mount(mp, opts)
	if !ok && withLabel {
		// 个别 WinFsp 构建不认 volname；降级重试一次。
		lg.Warnf("Mount with volname failed, retrying without label")
		opts = opts[:2]
		ok = host.Mount(mp, opts)
	}
	lg.Infof("Mount returned ok=%v after %s", ok, time.Since(t0))
	if !ok {
		os.Exit(2)
	}
}
