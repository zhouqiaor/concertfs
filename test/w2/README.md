# W2 真机测试脚本

PC 侧（Windows + Git Bash；curl.exe 8.x/OpenSSL 3.x/Python 3.13）。手机与 PC 同一 Wi-Fi LAN。

## 准备

1. App 启动共享后从屏幕（或 `files/logs/phonefs.log`）取：站点本地 IPv4、实际端口（首选 18443，占用则动态）、bearer token、SHA-256 指纹。
2. 10MB 压测文件（独立目录 PhoneFS-w2，收尾保留）：

```bash
ADB="/c/Programs/Android/Sdk/platform-tools/adb.exe"
"$ADB" shell mkdir -p /sdcard/PhoneFS-w2/中文目录
# 随机 10MB
head -c 10485760 /dev/urandom > test10mb.bin
"$ADB" push test10mb.bin /sdcard/PhoneFS-w2/test10mb.bin
"$ADB" shell ls -l /sdcard/PhoneFS-w2/
```

功能联调可用 `adb forward tcp:18443 tcp:<实际端口>`（HOST=127.0.0.1）；**吞吐数据必须走 Wi-Fi LAN，不得用 adb forward/reverse 数字代替**。

## 功能矩阵（200/206/401/416/If-Range/穿越等）

```bash
HOST=192.168.x.x PORT=18443 TOKEN=<token> PIN=<hex64> ./functional_test.sh
```

TLS 信任选择：功能脚本固定 `-k`（curl Schannel 不做固定），但用 openssl 抓证书算 SHA-256
与 `/v1/pair/info` 的 fingerprint 及 App 显示值三方比对（脚本第 0 步）；
**正式吞吐默认走 Python 指纹固定（--pin），不用 --insecure。**

## 吞吐（10MB × 1000 = 10GB）

```bash
# docId：
python -c "import base64;print('p:'+base64.urlsafe_b64encode(b'/storage/emulated/0/PhoneFS-w2/test10mb.bin').decode().rstrip('='))"

python throughput_test.py --host 192.168.x.x --port 18443 \
  --token <token> --pin <hex64> \
  --id 'p:<base64url>' --count 1000 --chunk 10485760 \
  --out results/w2-throughput-lan.json
```

指标：总量、墙钟耗时、平均 MiB/s、p50/p95/min/max 单块毫秒、HTTP 非 206/字节数不符的失败计数、
重连次数、协商 TLS 版本/套件、指纹校验结果。NFR-09 目标 ≥8MB/s 为工程目标非契约。

## 息屏 5 分钟

```bash
# 拉流期间另开：
HOST=192.168.x.x TOKEN=<token> ./screenoff_probe.sh results/screenoff-probe.log
# 手机侧：
"$ADB" logcat -c && "$ADB" logcat -v time phonefs.svc:I phonefs.audit:I phonefs.app:I phonefs.fixture:I '*:S' > results/logcat-screenoff.txt
# 用户：锁屏静置 5 分钟（探针跑完）
"$ADB" shell dumpsys deviceidle > results/dumpsys-deviceidle.txt
"$ADB" shell dumpsys activity services com.phonefs.source > results/dumpsys-service.txt
```

产物全部落 `results/`（提交摘要/关键日志，大日志按需保留）。
