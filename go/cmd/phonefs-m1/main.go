// Command phonefs-m1 把安卓 PhoneFS HTTPS 服务（W2, v1 协议）通过
// cgofuse + WinFsp 挂成 Windows 只读盘符——M1 demo1 最小桥。
//
// 例：
//
//	phonefs-m1 -mount P: -server https://192.168.43.1:18443 \
//	  -token-file token.txt -trace -log m1.log
//
// 只读 P0：Getattr/Readdir/Open/Read(1MiB Range)/Statfs；CTRL_C/CTRL_BREAK
// 优雅卸载。协议 conformance 债见 internal/httpfs/client.go 包注释。
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
	icon := flag.String("icon", "", "盘符图标 ico 绝对路径（覆盖 device.form_factor 自动选择；不写注册表）")
	trace := flag.Bool("trace", false, "记录每个 FUSE 回调与 Range 块")
	readOnly := flag.Bool("ro", false, "只读挂载（M1 行为：写回调全 EROFS、Access 拒写/删）；默认 rw（M2）")
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
	// 图标未被 -icon 显式覆盖时也取 form_factor；与卷标是否覆盖无关。
	volLabel := strings.TrimSpace(*label)
	formFactor := ""
	if volLabel == "" || strings.TrimSpace(*icon) == "" {
		piCtx, piCancel := context.WithTimeout(context.Background(), 15*time.Second)
		pi, piErr := cl.PairInfo(piCtx)
		piCancel()
		if piErr != nil {
			lg.Warnf("pair/info unavailable: %v", piErr)
		} else {
			formFactor = strings.ToLower(strings.TrimSpace(pi.DeviceFormFactor))
			if volLabel == "" && strings.TrimSpace(pi.DeviceName) != "" {
				volLabel = cleanVolumeLabel(pi.DeviceName)
			}
			lg.Infof("pair/info device.name=%q form_factor=%q fingerprint=%s", pi.DeviceName, formFactor, pi.Fingerprint)
		}
	}
	volLabel = cleanVolumeLabel(volLabel)
	if volLabel == "" {
		volLabel = "PhoneFS"
	}

	// 图标优先级：-icon 绝对路径 > pair/info form_factor 对应的 exe 相对资产。
	// other/缺字段/默认资产缺失均不生成 autorun.inf；显式 -icon 错误则快速失败。
	iconPath := strings.TrimSpace(*icon)
	if iconPath != "" && !filepath.IsAbs(iconPath) {
		lg.Fatalf("-icon must be an absolute path: %q", iconPath)
	}
	if iconPath == "" && (formFactor == "phone" || formFactor == "tablet" || formFactor == "pc") {
		exe, exeErr := os.Executable()
		if exeErr != nil {
			lg.Warnf("os.Executable failed; drive icon disabled: %v", exeErr)
		} else {
			iconPath = filepath.Join(filepath.Dir(exe), "assets", "icons", "drive_"+formFactor+".ico")
		}
	}
	if iconPath != "" {
		if st, statErr := os.Stat(iconPath); statErr != nil || !st.Mode().IsRegular() {
			if strings.TrimSpace(*icon) != "" {
				lg.Fatalf("-icon is not a readable regular file: %q (%v)", iconPath, statErr)
			}
			lg.Warnf("default %s icon unavailable (%v); autorun.inf disabled", formFactor, statErr)
			iconPath = ""
		}
	}
	if iconPath != "" {
		bfs.SetAutorunIcon(iconPath)
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

	// uid/gid=-1：归属映射到当前登录用户；M2 默认 rw，-ro 时加 ro 只读卷。
	optFlags := "uid=-1,gid=-1,umask=00"
	if *readOnly {
		optFlags += ",ro"
	}
	opts := []string{"-o", optFlags}
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
	lg.Infof("phonefs-m1 starting: mount=%s server=%s label=%q trace=%v cgofuse=v1.6.0 nocgo log=%s",
		mp, *server, volLabel, *trace, absLog)
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
