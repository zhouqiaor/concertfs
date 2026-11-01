// Command phonefs-m1 把安卓 PhoneFS HTTPS 服务通过 cgofuse + WinFsp 挂成 Windows 盘符。
// M2 默认 rw；-ro 保留 M1 只读挂载与写拒绝语义。
//
// 例：
//
//	phonefs-m1 -mount P: -server https://192.168.43.1:18443 \
//	  -token-file token.txt -trace -log m1.log
//
// 写路径：Create/Write/Truncate/Flush/Fsync/Release/Unlink/Rename/Mkdir/Rmdir/Utimens；
// CTRL_C/CTRL_BREAK 优雅卸载。协议 conformance 债见 internal/httpfs/client.go 包注释。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/winfsp/cgofuse/fuse"

	"phonefs/go/internal/httpfs"
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

// freeDriveLetter 从 start 起找第一个未占用盘符（GetLogicalDrives 位掩码）。
func freeDriveLetter(start byte) (string, error) {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	mask, _, _ := k32.NewProc("GetLogicalDrives").Call()
	for c := start; c <= 'Z'; c++ {
		if mask&(1<<(c-'A')) == 0 {
			return string(c) + ":", nil
		}
	}
	return "", fmt.Errorf("no free drive letter from %c:..Z:", start)
}

func main() {
	mount := flag.String("mount", "auto", "挂载盘符（如 P:）；auto 从 P: 起顺延")
	server := flag.String("server", httpfs.DefaultBaseURL, "PhoneFS HTTPS BaseURL")
	token := flag.String("token", "", "Bearer token（与 -token-file 二选一；也可用环境变量 PHONEFS_TOKEN）")
	tokenFile := flag.String("token-file", "", "从文件读 Bearer token（自动去空白）")
	logPath := flag.String("log", "", "日志文件路径（同时输出 stderr；空则仅 stderr）")
	label := flag.String("label", "", "卷标覆盖（默认自动取 pair/info 的 device.name，清洗后 ≤32 字符）")
	trace := flag.Bool("trace", false, "记录每个 FUSE 回调与 Range 块")
	readOnly := flag.Bool("ro", false, "只读挂载（M1 兼容模式）；默认 rw")
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

	bearer := resolveToken(*token, *tokenFile, lg)
	if bearer == "" {
		lg.Fatalf("missing bearer token: pass -token, -token-file, or PHONEFS_TOKEN")
	}

	mp := *mount
	if mp == "auto" {
		var err error
		if mp, err = freeDriveLetter('P'); err != nil {
			lg.Fatalf("%v", err)
		}
	}

	cl := httpfs.New(*server, bearer, lg)
	bfs := httpfs.NewBridgeFS(cl, lg, *trace, *readOnly)

	// 卷标默认 = 设备名（pair/info device.name，真机如 "HUAWEI TEQU-S2C"），
	// 使资源管理器「此电脑」显示 "HUAWEI TEQU-S2C (P:)"。拿不到回退 PhoneFS。
	volLabel := strings.TrimSpace(*label)
	if volLabel == "" {
		volLabel = "PhoneFS"
		piCtx, piCancel := context.WithTimeout(context.Background(), 15*time.Second)
		pi, piErr := cl.PairInfo(piCtx)
		piCancel()
		if piErr != nil || strings.TrimSpace(pi.DeviceName) == "" {
			lg.Warnf("pair/info unavailable (%v); falling back to volume label %q", piErr, volLabel)
		} else {
			volLabel = cleanVolumeLabel(pi.DeviceName)
			lg.Infof("volume label from device.name=%q -> %q (fingerprint=%s)", pi.DeviceName, volLabel, pi.Fingerprint)
		}
	}
	volLabel = cleanVolumeLabel(volLabel)
	if volLabel == "" {
		volLabel = "PhoneFS"
	}

	// 挂载前预检：roots 必须可取（401/断网在此快速失败，不会留下半挂卷）。
	preCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	err := bfs.Warmup(preCtx)
	cancel()
	if err != nil {
		var ae *httpfs.APIError
		if errors.As(err, &ae) && (ae.HTTPStatus == 401 || ae.HTTPStatus == 403) {
			lg.Fatalf("authentication rejected (%s): check -token/-token-file", ae.Code)
		}
		lg.Fatalf("preflight failed (server=%s): %v", *server, err)
	}

	host := fuse.NewFileSystemHost(bfs)
	host.SetCapCaseInsensitive(true) // Windows 语义；桥内已做大小写不敏感解析
	host.SetCapReaddirPlus(true)     // Readdir 直接带 stat，省一轮 Getattr
	host.SetCapDeleteAccess(true)    // 只读卷在 Access(DELETE_OK) 即拒删，避免假成功

	// uid/gid=-1：归属映射到当前登录用户；默认 rw，-ro 时让内核和桥双层拒写。
	opts := []string{"-o", "uid=-1,gid=-1,umask=00"}
	if *readOnly {
		opts[1] += ",ro"
	}
	withLabel := volLabel != ""
	if withLabel {
		opts = append(opts, "-o", "volname="+volLabel)
	}

	// 优雅卸载：CTRL_C / CTRL_BREAK → host.Unmount()，Mount 随即返回；第二次信号强退。
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
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
	lg.Infof("phonefs-m1 starting: mount=%s server=%s label=%q trace=%v ro=%v cgofuse=v1.6.0 nocgo log=%s",
		mp, *server, volLabel, *trace, *readOnly, absLog)
	t0 := time.Now()
	ok := host.Mount(mp, opts)
	if !ok && withLabel {
		lg.Warnf("Mount with volname failed, retrying without label")
		opts = opts[:2]
		ok = host.Mount(mp, opts)
	}
	lg.Infof("Mount returned ok=%v after %s", ok, time.Since(t0))
	if !ok {
		os.Exit(2)
	}
}

// cleanVolumeLabel 清洗卷标：去掉 Windows 卷标非法字符（\ / : * ? " < > | 与
// 控制字符）、trim 空白、限制 32 字符（NTFS 卷标上限），避免设备名带怪字符时挂载失败。
// 逗号与等号也必须剔除：volname 原样进 WinFsp C 侧 fuse_opt_parse 按逗号分词，
// 不清洗则 "-label 'X,rw=Y'" 类输入会被解析成额外挂载选项（注入面，Colossus P1-2）。
func cleanVolumeLabel(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|,=`, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > 32 {
		out = string([]rune(out)[:32])
	}
	return out
}

func resolveToken(inline, file string, lg *levelLogger) string {
	switch {
	case inline != "":
		return strings.TrimSpace(inline)
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			lg.Fatalf("read token file %q: %v", file, err)
		}
		return strings.TrimSpace(string(b))
	default:
		return strings.TrimSpace(os.Getenv("PHONEFS_TOKEN"))
	}
}
