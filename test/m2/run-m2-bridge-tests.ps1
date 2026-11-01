# PhoneFS M2 rw 桥真机验收（在默认 rw 挂载的盘符上运行）
#   powershell -NoProfile -ExecutionPolicy Bypass -File run-m2-bridge-tests.ps1 -Drive P: -Tag round1
# 所有写操作限定 内部存储/PhoneFS-m2-bridge；SHA 同时校验 FUSE 读回与 adb 手机直读。
# Shell.Application.CopyHere 覆盖 Explorer 同源真实复制；脚本不含也不读取 Bearer token。
param(
    [ValidatePattern('^[A-Za-z]:$')]
    [string]$Drive = 'P:',
    [string]$Tag = 'run',
    [string]$Serial = '192.168.43.1:5555',
    [string]$Adb = 'C:/Programs/Android/Sdk/platform-tools/adb.exe'
)
$ErrorActionPreference = 'Stop'
$env:MSYS_NO_PATHCONV = '1'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
if ($Drive.TrimEnd(':') -ieq $env:SystemDrive.TrimEnd(':')) { throw "refusing system drive $Drive" }
$vol = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$Drive'" -ErrorAction SilentlyContinue
if (-not $vol -or $vol.FileSystem -ne 'FUSE') { throw "drive $Drive is not a mounted FUSE volume" }

$storageName = -join @([char]0x5185,[char]0x90E8,[char]0x5B58,[char]0x50A8)
$storage = Join-Path ($Drive + '\') $storageName
$rootName = 'PhoneFS-m2-bridge'
$root = Join-Path $storage $rootName
$androidRoot = '/storage/emulated/0/' + $rootName
$results = New-Object System.Collections.ArrayList
$swTotal = [Diagnostics.Stopwatch]::StartNew()
function Add-Result($id, $name, $ok, $detail) {
    [void]$results.Add([pscustomobject]@{ id=$id; name=$name; pass=[bool]$ok;
        detail=($detail -replace "`r?`n",' '); ts=(Get-Date).ToString('HH:mm:ss.fff') })
}
function Case($id, $name, $sb) {
    try { $d = & $sb; if ($null -eq $d) { $d = 'ok' }; Add-Result $id $name $true $d }
    catch { Add-Result $id $name $false ("{0}: {1}" -f $_.Exception.GetType().Name,$_.Exception.Message) }
}
function Must($cond, $msg) { if (-not $cond) { throw $msg } }
function Hash-Bytes([byte[]]$bytes) {
    $sha = [Security.Cryptography.SHA256]::Create()
    return ([BitConverter]::ToString($sha.ComputeHash($bytes))).Replace('-','')
}
function Server-Sha([string]$relative) {
    $remote = $androidRoot + '/' + ($relative -replace '\\','/')
    $line = (& $Adb -s $Serial shell sha256sum $remote 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $line) { return '' }
    return ($line.ToString().Split()[0].ToUpperInvariant())
}
function Must-ServerSha([string]$relative, [string]$want) {
    $got = ''
    for ($i=0; $i -lt 40; $i++) {
        $got = Server-Sha $relative
        if ($got -eq $want) { return $got }
        Start-Sleep -Milliseconds 250
    }
    throw "server sha $relative=$got want=$want"
}
function Write-AllAndSync([string]$path, [byte[]]$bytes) {
    $fs = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try { $fs.Write($bytes,0,$bytes.Length); $fs.Flush($true) } finally { $fs.Close() }
}

# 只清自己的隔离根；若上次失败留下内容，也通过被测 rw 桥删除。
if (Test-Path -LiteralPath $root) { Remove-Item -LiteralPath $root -Recurse -Force }
Case '01' 'mkdir-isolated-root' {
    New-Item -ItemType Directory -Path $root | Out-Null
    Must (Test-Path -LiteralPath $root -PathType Container) 'root missing'
    $root
}
$file = Join-Path $root 'random.bin'
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$data = New-Object byte[] 2500000
$rng.GetBytes($data)
$dataSha = Hash-Bytes $data
Case '02' 'create-multichunk-write-fsync-server-sha' {
    Write-AllAndSync $file $data
    $got = Must-ServerSha 'random.bin' $dataSha
    "2500000B (3 chunks) server sha256=$got"
}
Case '03' 'fuse-readback-sha' {
    $bytes = [IO.File]::ReadAllBytes($file)
    Must ($bytes.Length -eq $data.Length) "length=$($bytes.Length)"
    $got = Hash-Bytes $bytes
    Must ($got -eq $dataSha) "sha=$got want=$dataSha"
    "FUSE readback sha256=$got"
}
Case '04' 'other-open-handle-cache-invalidation' {
    $reader = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Read,[IO.FileShare]::ReadWrite)
    try {
        $before = New-Object byte[] 65536
        Must ($reader.Read($before,0,$before.Length) -eq $before.Length) 'initial short read'
        $patch = New-Object byte[] 65536
        $rng.GetBytes($patch)
        $writer = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::ReadWrite)
        try { $writer.Position = 0; $writer.Write($patch,0,$patch.Length); $writer.Flush($true) } finally { $writer.Close() }
        [Array]::Copy($patch,0,$data,0,$patch.Length)
        $script:dataSha = Hash-Bytes $data
        Must-ServerSha 'random.bin' $dataSha | Out-Null
        $reader.Position = 0
        $after = New-Object byte[] 65536
        Must ($reader.Read($after,0,$after.Length) -eq $after.Length) 'reread short read'
        Must ((Hash-Bytes $after) -eq (Hash-Bytes $patch)) 'existing read handle returned stale Range block'
        'existing handle dropped old block after node version changed'
    } finally { $reader.Close() }
}
Case '05' 'random-unaligned-write-and-extension' {
    $patch = New-Object byte[] 100003
    $rng.GetBytes($patch)
    $tail = New-Object byte[] 50017
    $rng.GetBytes($tail)
    $fs = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try {
        $fs.Position = 1000003; $fs.Write($patch,0,$patch.Length)
        $fs.Position = $data.Length; $fs.Write($tail,0,$tail.Length); $fs.Flush($true)
    } finally { $fs.Close() }
    [Array]::Copy($patch,0,$data,1000003,$patch.Length)
    $oldLen = $data.Length
    [Array]::Resize([ref]$data,$oldLen + $tail.Length)
    [Array]::Copy($tail,0,$data,$oldLen,$tail.Length)
    $script:dataSha = Hash-Bytes $data
    Must-ServerSha 'random.bin' $dataSha | Out-Null
    Must ((Hash-Bytes ([IO.File]::ReadAllBytes($file))) -eq $dataSha) 'FUSE sha after random write'
    "unaligned overwrite=100003B extension=50017B size=$($data.Length) sha=$dataSha"
}
Case '06' 'nonzero-truncate-enosys' {
    $denied = $false
    $fs = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try { try { $fs.SetLength(123456) } catch { $denied = $true } } finally { $fs.Close() }
    Must $denied 'nonzero truncate unexpectedly succeeded'
    Must ((Server-Sha 'random.bin') -eq $dataSha) 'content changed after rejected truncate'
    'SetLength(123456) rejected; server bytes unchanged'
}
Case '07' 'zero-truncate-supported' {
    $fs = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try { $fs.SetLength(0); $fs.Flush($true) } finally { $fs.Close() }
    $emptySha = Hash-Bytes ([byte[]]@())
    Must-ServerSha 'random.bin' $emptySha | Out-Null
    Must ((Get-Item -LiteralPath $file).Length -eq 0) 'FUSE size not zero'
    $payload = [Text.Encoding]::UTF8.GetBytes('PhoneFS M2 rename/move payload')
    $script:data = $payload
    $script:dataSha = Hash-Bytes $payload
    $fs = [IO.File]::Open($file,[IO.FileMode]::Open,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try { $fs.Write($payload,0,$payload.Length); $fs.Flush($true) } finally { $fs.Close() }
    Must-ServerSha 'random.bin' $dataSha | Out-Null
    "SetLength(0)+empty upload passed; rewrote $($payload.Length)B"
}
$renamed = Join-Path $root 'renamed.bin'
Case '08' 'rename-file' {
    [IO.File]::Move($file,$renamed)
    Must (-not (Test-Path -LiteralPath $file)) 'old path remains'
    Must ((Hash-Bytes ([IO.File]::ReadAllBytes($renamed))) -eq $dataSha) 'renamed content mismatch'
    Must-ServerSha 'renamed.bin' $dataSha | Out-Null
    'old id/path gone; renamed bytes match'
}
$subdir = Join-Path $root 'moved'
$moved = Join-Path $subdir 'renamed.bin'
Case '09' 'mkdir-and-move-file' {
    New-Item -ItemType Directory -Path $subdir | Out-Null
    [IO.File]::Move($renamed,$moved)
    Must (-not (Test-Path -LiteralPath $renamed)) 'source remains after move'
    Must-ServerSha 'moved/renamed.bin' $dataSha | Out-Null
    'cross-directory PATCH parent_id; server bytes match'
}
Case '10' 'utimens-mtime' {
    $when = [DateTime]::UtcNow.AddDays(-3)
    $when = $when.Date.AddHours($when.Hour).AddMinutes($when.Minute).AddSeconds($when.Second)
    [IO.File]::SetLastWriteTimeUtc($moved,$when)
    $actual = [IO.File]::GetLastWriteTimeUtc($moved)
    Must ([math]::Abs(($actual-$when).TotalSeconds) -le 2) "mtime=$actual want=$when"
    "mtime=$($actual.ToString('o'))"
}
Case '11' 'create-conflict-eexist' {
    $denied = $false
    try { Write-AllAndSync $moved ([byte[]]@(1)) } catch { $denied = $true }
    Must $denied 'CreateNew existing file succeeded'
    Must-ServerSha 'moved/renamed.bin' $dataSha | Out-Null
    'CreateNew existing file failed; bytes unchanged'
}
Case '12' 'nonempty-rmdir-enotempty' {
    $denied = $false
    try { Remove-Item -LiteralPath $subdir -ErrorAction Stop } catch { $denied = $true }
    Must $denied 'nonempty directory removal succeeded'
    Must (Test-Path -LiteralPath $moved) 'child vanished'
    'nonempty Rmdir rejected; child intact'
}
Case '13' 'shell-explorer-copyhere' {
    $localDir = Join-Path $env:TEMP ('PhoneFS-m2-local-' + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $localDir | Out-Null
    try {
        $local = Join-Path $localDir 'explorer-copy.bin'
        $copyData = New-Object byte[] 3200123
        $rng.GetBytes($copyData)
        [IO.File]::WriteAllBytes($local,$copyData)
        $want = Hash-Bytes $copyData
        $shell = New-Object -ComObject Shell.Application
        $ns = $shell.Namespace($root)
        Must ($null -ne $ns) 'Shell destination namespace null'
        $ns.CopyHere($local,0x10)
        $dest = Join-Path $root 'explorer-copy.bin'
        $found = $false
        for ($i=0; $i -lt 80; $i++) { if (Test-Path -LiteralPath $dest) { $found=$true; break }; Start-Sleep -Milliseconds 250 }
        Must $found 'CopyHere destination did not appear'
        Must-ServerSha 'explorer-copy.bin' $want | Out-Null
        Must ((Hash-Bytes ([IO.File]::ReadAllBytes($dest))) -eq $want) 'CopyHere FUSE readback mismatch'
        "Shell.Application.CopyHere 3200123B sha=$want"
    } finally { Remove-Item -LiteralPath $localDir -Recurse -Force -ErrorAction SilentlyContinue }
}
Case '14' 'unlink-and-rmdir' {
    Remove-Item -LiteralPath $moved -Force
    Must (-not (Test-Path -LiteralPath $moved)) 'file still visible after unlink'
    Remove-Item -LiteralPath $subdir -Force
    Must (-not (Test-Path -LiteralPath $subdir)) 'empty dir still visible after rmdir'
    'Unlink + empty Rmdir passed'
}
Case '15' 'cleanup-isolated-tree' {
    Remove-Item -LiteralPath $root -Recurse -Force
    Must (-not (Test-Path -LiteralPath $root)) 'test root remains'
    $remote = (& $Adb -s $Serial shell test ! -e $androidRoot 2>$null)
    Must ($LASTEXITCODE -eq 0) 'Android isolated root remains'
    'all M2 bridge test objects removed through FUSE'
}

$swTotal.Stop()
$pass = @($results | Where-Object pass).Count
$fail = $results.Count - $pass
$outDir = $PSScriptRoot
$json = Join-Path $outDir "bridge-results-$Tag.json"
$txt = Join-Path $outDir "bridge-results-$Tag.txt"
$results | ConvertTo-Json -Depth 4 | Out-File -Encoding utf8 $json
$lines = @("PhoneFS M2 rw bridge - $Tag - $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') - drive=$Drive serial=$Serial",
    ("PASS {0}/{1} FAIL {2} TOTAL {3}s" -f $pass,$results.Count,$fail,[math]::Round($swTotal.Elapsed.TotalSeconds,1)))
$lines += ($results | ForEach-Object { "{0} [{1}] {2,-42} {3} | {4}" -f $(if($_.pass){'PASS'}else{'FAIL'}),$_.id,$_.name,$_.ts,$_.detail })
$lines | Out-File -Encoding utf8 $txt
Get-Content $txt
"SUMMARY: PASS $pass / $($results.Count), FAIL $fail"
if ($fail -gt 0) { exit 1 }
