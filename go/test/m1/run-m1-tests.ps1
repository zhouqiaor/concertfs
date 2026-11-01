# PhoneFS M1 只读桥真机验收（在已挂载的 phonefs-m1 盘符上运行）
#   powershell -NoProfile -ExecutionPolicy Bypass -File test/m1/run-m1-tests.ps1 -Drive P: -Tag round1
# 产出：runs/m1-results-<Tag>.json + .txt；参考字节（curl/adb 直取，不经 FUSE）在 runs/ref-<Tag>/。
# 硬化：固定作用域 $Drive 内只读操作；拒绝系统盘；预检 FUSE 卷才继续。
# 注意：所有中文/emoji 名称一律码点构造，规避 PS5.1 无 BOM 脚本解码问题（W1 同款纪律）。
param(
    [ValidatePattern('^[A-Za-z]:$')]
    [string]$Drive = 'P:',
    [string]$Tag = 'run',
    [string]$Serial = '192.168.43.1:5555',
    [string]$BaseUrl = 'https://192.168.43.1:18443',
    [string]$Adb = 'C:/Programs/Android/Sdk/platform-tools/adb.exe'
)
$ErrorActionPreference = 'Stop'
$env:MSYS_NO_PATHCONV = '1'  # adb 的 Android 路径参数不能被 Git Bash/MSYS 转换
# curl 的 JSON 是 UTF-8；PS5.1 默认按 OEM(GBK) 解码外部程序 stdout 会把中文名解坏
[Console]::OutputEncoding = [Text.Encoding]::UTF8

# 拒绝系统盘（只读验收不删任何对象，但仍拒绝指向 OS 卷）
if ($Drive.TrimEnd(':') -ieq $env:SystemDrive.TrimEnd(':')) {
    throw "refusing to run on system drive $Drive (SystemDrive=$env:SystemDrive)"
}
$runsDir = Join-Path $PSScriptRoot 'runs'
$refDir = Join-Path $runsDir ("ref-" + $Tag)
New-Item -ItemType Directory -Force -Path $refDir | Out-Null

# 盘符预检：必须存在且为 FUSE 卷（phonefs-m1），防止误指真实盘
$vol = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$($Drive.TrimEnd('\'))'" -ErrorAction SilentlyContinue
if (-not $vol) { throw "drive $Drive not found in Win32_LogicalDisk; aborting" }
if ($vol.FileSystem -ne 'FUSE') { throw "drive $Drive FileSystem='$($vol.FileSystem)' is not FUSE; only phonefs-m1 volume allowed" }

$swTotal = [Diagnostics.Stopwatch]::StartNew()
$results = New-Object System.Collections.ArrayList
function Add-Result($id, $category, $ok, $detail) {
    [void]$results.Add([pscustomobject]@{
        id = $id; category = $category; pass = [bool]$ok
        detail = ($detail -replace "`r?`n", ' '); ts = (Get-Date).ToString('HH:mm:ss.fff')
    })
}
function Case($id, $category, $name, $sb) {
    try {
        $d = & $sb
        if ($null -eq $d) { $d = 'ok' }
        Add-Result $id ("{0}/{1}" -f $category, $name) $true $d
    } catch {
        Add-Result $id ("{0}/{1}" -f $category, $name) $false ("{0}: {1}" -f $_.Exception.GetType().Name, $_.Exception.Message)
    }
}
function Must($cond, $msg) { if (-not $cond) { throw $msg } }

# ---------- 名称（码点构造） ----------
$storageName = -join @([char]0x5185,[char]0x90E8,[char]0x5B58,[char]0x50A8)   # 内部存储
$imgName     = -join @([char]0x56FE,[char]0x7247)                              # 图片
$cnDirName   = -join @([char]0x4E2D,[char]0x6587,[char]0x76EE,[char]0x5F55)  # 中文目录
$txtName     = (-join @([char]0x6D4B,[char]0x8BD5)) + ' ' + (-join @([char]0x6587,[char]0x4EF6)) + '.txt'  # 测试 文件.txt
$expectedRoots = @(
    (-join @([char]0x56FE,[char]0x7247)),                                      # 图片
    (-join @([char]0x89C6,[char]0x9891)),                                      # 视频
    (-join @([char]0x97F3,[char]0x9891)),                                      # 音频
    (-join @([char]0x6587,[char]0x6863)),                                      # 文档
    (-join @([char]0x538B,[char]0x7F29,[char]0x5305)),                         # 压缩包
    (-join @([char]0x4E0B,[char]0x8F7D)),                                      # 下载
    (-join @([char]0x622A,[char]0x56FE)),                                      # 截图
    $storageName
)
$storage = Join-Path ($Drive + '\') $storageName
$w2 = Join-Path $storage 'PhoneFS-w2'
$txtPath = Join-Path $w2 $txtName
$binPath = Join-Path $w2 'test10mb.bin'
$cnDir = Join-Path $w2 $cnDirName

# ---------- 参考字节（curl 直取服务端，不经 FUSE） ----------
$token = (& $Adb -s $Serial shell cat /data/data/com.phonefs.source/files/identity/token).Trim()
Must ($token.Length -gt 10) 'token read failed'
$pairRaw = & curl.exe -sk --max-time 10 ($BaseUrl + '/v1/pair/info') | ConvertFrom-Json
$deviceName = $pairRaw.device.name
$listW2 = & curl.exe -sk --max-time 15 -H "Authorization: Bearer $token" -G ($BaseUrl + '/v1/fs/list') --data-urlencode 'id=p:L3N0b3JhZ2UvZW11bGF0ZWQvMC9QaG9uZUZTLXcy' | ConvertFrom-Json
$binEntry = $listW2.entries | Where-Object { $_.name -eq 'test10mb.bin' }
$txtEntry = $listW2.entries | Where-Object { $_.name -ceq $txtName }
Must ($binEntry.size -eq 10485760) "server bin size=$($binEntry.size)"
Must ($txtEntry.size -eq 40) "server txt size=$($txtEntry.size)"
function Fetch-Ref($id, $range, $outName) {
    $out = Join-Path $refDir $outName
    $args = @('-sk','--max-time','30','-H',"Authorization: Bearer $token",'-H',"Range: bytes=$range",'-G',($BaseUrl + '/v1/fs/content'),'--data-urlencode',"id=$id",'-o',$out)
    & curl.exe @args | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "curl ref $outName failed rc=$LASTEXITCODE" }
    return $out
}
$refHead = Fetch-Ref $binEntry.id '0-1023' 'bin-head-1k.bin'
$refTail = Fetch-Ref $binEntry.id '10484736-10485759' 'bin-tail-1k.bin'
$refMid  = Fetch-Ref $binEntry.id '5242880-5246976' 'bin-mid-4k.bin'  # 4097B：从 +1 偏移切 4096B 给未对齐用例
$refTxt  = Fetch-Ref $txtEntry.id '0-39' 'txt-full.bin'

function Read-At($path, [int64]$offset, [int]$len) {
    $fs = [IO.File]::Open($path, 'Open', 'Read', 'ReadWrite')
    try {
        [void]$fs.Seek($offset, 'Begin')
        $buf = New-Object byte[] $len
        $read = 0
        while ($read -lt $len) {
            $n = $fs.Read($buf, $read, $len - $read)
            if ($n -le 0) { break }
            $read += $n
        }
        return ,@($buf, $read)
    } finally { $fs.Close() }
}
function Hash-Bytes($bytes) {
    $sha = [Security.Cryptography.SHA256]::Create()
    return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-','')
}

# ---------- 01 顶层 8 中文根 ----------
Case 01 roots 'eight-chinese-roots' {
    $names = @(Get-ChildItem -LiteralPath ($Drive + '\') -Force | ForEach-Object { $_.Name })
    Must ($names.Count -ge 8) "root count=$($names.Count)"
    $missing = @($expectedRoots | Where-Object { $names -notcontains $_ })
    Must ($missing.Count -eq 0) "missing roots: $($missing -join ',')"
    "roots=$($names.Count) all 8 expected present"
}

# ---------- 02 卷标=设备名 / 固定盘 / 容量=statfs ----------
Case 02 volume 'device-label-and-statfs' {
    $v = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$($Drive.TrimEnd('\'))'"
    # WinFsp 卷不在 Storage WMI provider 里（Get-Volume 查不到），卷标以 Win32_LogicalDisk 为准
    $clean = ($deviceName -replace '[\\\/:*?"<>|]','').Trim()
    if ($clean.Length -gt 32) { $clean = $clean.Substring(0,32) }
    Must ($v.VolumeName -ceq $clean) "label='$($v.VolumeName)' want='$clean' (device.name='$deviceName')"
    Must ($v.DriveType -eq 3) "DriveType=$($v.DriveType) want 3(fixed)"
    $sf = & curl.exe -sk --max-time 10 -H "Authorization: Bearer $token" ($BaseUrl + '/v1/fs/statfs') | ConvertFrom-Json
    Must ($v.Size -eq $sf.total_bytes) "Size=$($v.Size) != statfs total=$($sf.total_bytes)"
    "label='$($v.VolumeName)' fixed DriveType=3 Size=$($v.Size)==statfs Free=$($v.FreeSpace)"
}

# ---------- 03 进入 PhoneFS-w2 见 fixture ----------
Case 03 tree 'phonefs-w2-fixtures' {
    $items = Get-ChildItem -LiteralPath $w2
    $names = @($items | ForEach-Object { $_.Name })
    Must ($names -ccontains $txtName) "txt not found: $($names -join '|')"
    Must ($names -contains 'test10mb.bin') 'bin missing'
    Must ($names -ccontains $cnDirName) 'cn dir missing'
    $b = $items | Where-Object { $_.Name -eq 'test10mb.bin' }
    Must ($b.Length -eq 10485760) "bin length=$($b.Length)"
    $t = $items | Where-Object { $_.Name -ceq $txtName }
    Must ($t.Length -eq 40) "txt length=$($t.Length)"
    $wantMtime = [DateTimeOffset]::FromUnixTimeMilliseconds($txtEntry.modified_ms).LocalDateTime
    $delta = [math]::Abs(($t.LastWriteTime - $wantMtime).TotalSeconds)
    Must ($delta -le 2) "mtime delta=${delta}s"
    "w2 has txt(40B)+bin(10MiB)+中文目录; mtime matches server modified_ms"
}

# ---------- 04 文本内容逐字节正确 ----------
Case 04 content 'txt-40b-exact' {
    $bytes = [IO.File]::ReadAllBytes($txtPath)
    Must ($bytes.Length -eq 40) "len=$($bytes.Length)"
    $h1 = Hash-Bytes $bytes
    $h2 = (Get-FileHash $refTxt -Algorithm SHA256).Hash
    Must ($h1 -eq $h2) "sha mismatch FUSE=$h1 ref=$h2"
    $text = [Text.Encoding]::UTF8.GetString($bytes)
    Must ($text.StartsWith('PhoneFS W2 ')) "unexpected text head"
    "40 bytes sha256=$($h1.Substring(0,16))... == server; UTF8 decodes"
}

# ---------- 05 中文目录可进入（含 emoji 文件） ----------
Case 05 names 'chinese-dir-and-emoji' {
    $items = Get-ChildItem -LiteralPath $cnDir
    Must ($items.Count -ge 1) 'cn dir empty'
    $emoji = $items | Where-Object { $_.Name -match '\.bin$' }
    $md = $items | Where-Object { $_.Name -eq 'a b c.md' }
    Must ($null -ne $md) 'a b c.md missing'
    if ($emoji) {
        $b = [IO.File]::ReadAllBytes($emoji[0].FullName)
        "cn dir: $($items.Count) entries incl emoji-name bin($($b.Length)B) + 'a b c.md'($($md.Length)B)"
    } else {
        "cn dir: $($items.Count) entries (a b c.md=$($md.Length)B); no .bin emoji file"
    }
}

# ---------- 06 10MB 头 1KB 随机读 ----------
Case 06 random 'bin-head-1k' {
    $r = Read-At $binPath 0 1024
    Must ($r[1] -eq 1024) "read=$($r[1])"
    $h = Hash-Bytes $r[0]
    $ref = (Get-FileHash $refHead -Algorithm SHA256).Hash
    Must ($h -eq $ref) 'head sha mismatch'
    "head 1KiB sha256 == server range ref"
}

# ---------- 07 10MB 尾 1KB（Seek 到末尾） ----------
Case 07 random 'bin-tail-1k' {
    $r = Read-At $binPath 10484736 1024
    Must ($r[1] -eq 1024) "read=$($r[1])"
    $h = Hash-Bytes $r[0]
    $ref = (Get-FileHash $refTail -Algorithm SHA256).Hash
    Must ($h -eq $ref) 'tail sha mismatch'
    "tail 1KiB @10484736 sha256 == server range ref"
}

# ---------- 08 中段 4KB（未对齐随机偏移） ----------
Case 08 random 'bin-mid-unaligned' {
    $r = Read-At $binPath 5242881 4096   # +1 非对齐，强制跨块切片
    Must ($r[1] -eq 4096) "read=$($r[1])"
    $refBytes = [IO.File]::ReadAllBytes($refMid)
    $expect = New-Object byte[] 4096
    [Array]::Copy($refBytes, 1, $expect, 0, 4096)
    $h = Hash-Bytes $r[0]; $h2 = Hash-Bytes $expect
    Must ($h -eq $h2) 'mid sha mismatch'
    "mid 4KiB @5242881 (unaligned) sha256 == server ref"
}

# ---------- 09 整文件顺序流读 10MB（多块+缓存路径） ----------
Case 09 stream 'bin-full-10mb-sha256' {
    $sw = [Diagnostics.Stopwatch]::StartNew()
    $h1 = (Get-FileHash $binPath -Algorithm SHA256).Hash
    $sw.Stop()
    $refFull = Join-Path $refDir 'bin-full.bin'
    & curl.exe -sk --max-time 60 -H "Authorization: Bearer $token" -G ($BaseUrl + '/v1/fs/content') --data-urlencode "id=$($binEntry.id)" -o $refFull | Out-Null
    $h2 = (Get-FileHash $refFull -Algorithm SHA256).Hash
    Must ($h1 -eq $h2) "full sha mismatch FUSE=$h1 ref=$h2"
    "10MiB full-stream sha256 match in $([math]::Round($sw.Elapsed.TotalSeconds,2))s"
}

# ---------- 10 分类根（501）按空目录可进入 ----------
Case 10 category 'cat-root-empty-501' {
    $img = Join-Path ($Drive + '\') $imgName
    Must (Test-Path $img) 'cat root missing'
    $c = @(Get-ChildItem -LiteralPath $img).Count
    Must ($c -eq 0) "cat root entries=$c (W2 501 should present empty)"
    'category root opens and enumerates empty (W2 501 NOT_SUPPORTED mapped)'
}

# ---------- 11 大小写不敏感 ----------
Case 11 names 'case-insensitive' {
    $p = Join-Path (Join-Path $storage 'PHONEFS-W2') 'TEST10MB.BIN'
    Must (Test-Path $p) 'upper-case path not resolved'
    'PHONEFS-W2/TEST10MB.BIN resolves'
}

# ---------- 12 只读卷：创建被拒绝 ----------
Case 12 readonly 'create-denied' {
    $denied = $false
    try { [IO.File]::WriteAllText((Join-Path $w2 'm1-should-not-exist.txt'), 'x') } catch { $denied = $true }
    Must $denied 'write was NOT denied'
    Must (-not (Test-Path (Join-Path $w2 'm1-should-not-exist.txt'))) 'file appeared on read-only volume'
    'create on read-only volume denied, nothing created'
}

# ---------- 13 只读卷：删除被拒绝 ----------
Case 13 readonly 'delete-denied' {
    $denied = $false
    try { Remove-Item -LiteralPath $txtPath -ErrorAction Stop } catch { $denied = $true }
    Must $denied 'delete was NOT denied'
    Must (Test-Path $txtPath) 'fixture vanished'
    'delete denied; fixture intact'
}

# ---------- 14 不存在路径 ENOENT ----------
Case 14 errors 'missing-path-enonet' {
    $bad = Join-Path $w2 'no-such-file-m1.xyz'
    Must (-not (Test-Path $bad)) 'phantom file exists'
    $denied = $false
    try { [IO.File]::ReadAllBytes($bad) | Out-Null } catch { $denied = $true }
    Must $denied 'open of missing file succeeded'
    'missing path -> not found (ENOENT mapped)'
}

# ---------- 15 EOF 语义：读到末尾返回 0 ----------
Case 15 errors 'eof-zero-read' {
    $fs = [IO.File]::Open($binPath, 'Open', 'Read', 'ReadWrite')
    try {
        [void]$fs.Seek(10485760, 'Begin')
        $buf = New-Object byte[] 128
        $n = $fs.Read($buf, 0, 128)
        Must ($n -eq 0) "read past EOF returned $n"
        [void]$fs.Seek(10485750, 'Begin')
        $n2 = $fs.Read($buf, 0, 128)
        Must ($n2 -eq 10) "tail read=$n2 want 10"
        "EOF read=0; last-10-bytes read=10"
    } finally { $fs.Close() }
}

# ---------- 16 Shell.Application 视角（资源管理器同源） ----------
Case 16 shell 'shell-application-browse' {
    $shell = New-Object -ComObject Shell.Application
    $ns = $shell.Namespace($Drive + '\')
    Must ($null -ne $ns) 'namespace null'
    $names = @($ns.Items() | ForEach-Object { $_.Name })
    Must ($names -ccontains $storageName) "storage not visible in shell: $($names -join ',')"
    "shell sees $($names.Count) root items incl 内部存储"
}

# ---------- 汇总 ----------
$swTotal.Stop()
$pass = ($results | Where-Object pass).Count
$fail = $results.Count - $pass
$json = Join-Path $runsDir "m1-results-$Tag.json"
$txt  = Join-Path $runsDir "m1-results-$Tag.txt"
$results | ConvertTo-Json -Depth 4 | Out-File -Encoding utf8 $json
$lines = @()
$lines += "PhoneFS M1 results - $Tag - $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') - drive=$Drive device=$deviceName"
$lines += ("PASS {0}/{1}  FAIL {2}  TOTAL {3}s" -f $pass, $results.Count, $fail, [math]::Round($swTotal.Elapsed.TotalSeconds,1))
$lines += ($results | ForEach-Object { "{0} [{1}] {2,-28} {3} | {4}" -f $(if($_.pass){'PASS'}else{'FAIL'}), $_.id, $_.category, $_.ts, $_.detail })
$lines | Out-File -Encoding utf8 $txt
Get-Content $txt
""
"SUMMARY: PASS $pass / $($results.Count), FAIL $fail, total $([math]::Round($swTotal.Elapsed.TotalSeconds,1))s"
if ($fail -gt 0) { exit 1 }

