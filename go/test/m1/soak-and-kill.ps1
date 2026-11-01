# PhoneFS M1 存活与强杀清理验收：
#   1) 在已挂载的 phonefs-m1 盘符上持续遍历/读取 >= -Seconds（默认 300s）；
#   2) Stop-Process -Force（等同 kill -9）桥进程；
#   3) 断言盘符在 -UnmountTimeoutSec 内干净消失。
# 用法：
#   powershell -NoProfile -ExecutionPolicy Bypass -File test/m1/soak-and-kill.ps1 -Drive P: -BridgePid 12345 -Tag soak1
# 硬化：仅只读遍历 $Drive；拒绝系统盘；预检 FUSE 卷；只杀显式传入且映像名为 phonefs-m1 的 PID。
param(
    [ValidatePattern('^[A-Za-z]:$')]
    [string]$Drive = 'P:',
    [Parameter(Mandatory=$true)][int]$BridgePid,
    [int]$Seconds = 300,
    [int]$UnmountTimeoutSec = 30,
    [string]$Tag = 'soak'
)
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8
if ($Drive.TrimEnd(':') -ieq $env:SystemDrive.TrimEnd(':')) { throw "refusing system drive $Drive" }
$vol = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$($Drive.TrimEnd('\'))'" -ErrorAction SilentlyContinue
if (-not $vol -or $vol.FileSystem -ne 'FUSE') { throw "drive $Drive is not a mounted FUSE volume" }

$proc = Get-Process -Id $BridgePid -ErrorAction SilentlyContinue
if (-not $proc) { throw "bridge pid $BridgePid not running" }
if ($proc.ProcessName -ne 'phonefs-m1') { throw "pid $BridgePid is '$($proc.ProcessName)', not phonefs-m1; refusing to kill" }

$runsDir = Join-Path $PSScriptRoot 'runs'
New-Item -ItemType Directory -Force -Path $runsDir | Out-Null
$storageName = -join @([char]0x5185,[char]0x90E8,[char]0x5B58,[char]0x50A8)
$txtName = (-join @([char]0x6D4B,[char]0x8BD5)) + ' ' + (-join @([char]0x6587,[char]0x4EF6)) + '.txt'
$storage = Join-Path ($Drive + '\') $storageName
$w2 = Join-Path $storage 'PhoneFS-w2'
$txtPath = Join-Path $w2 $txtName

$deadline = (Get-Date).AddSeconds($Seconds)
$iter = 0; $readOK = 0; $errors = New-Object System.Collections.ArrayList
$sw = [Diagnostics.Stopwatch]::StartNew()
Write-Output ("soak start {0} drive={1} bridgePid={2} seconds={3}" -f (Get-Date -Format 'HH:mm:ss'), $Drive, $BridgePid, $Seconds)
while ((Get-Date) -lt $deadline) {
    $iter++
    try {
        $roots = @(Get-ChildItem -LiteralPath ($Drive + '\') -Force)
        if ($roots.Count -lt 8) { throw "root count=$($roots.Count)" }
        $items = Get-ChildItem -LiteralPath $w2
        $bytes = [IO.File]::ReadAllBytes($txtPath)
        if ($bytes.Length -ne 40) { throw "txt len=$($bytes.Length)" }
        $readOK++
    } catch {
        [void]$errors.Add(("{0} iter={1}: {2}" -f (Get-Date -Format 'HH:mm:ss'), $iter, $_.Exception.Message))
    }
    Start-Sleep -Seconds 5
}
$soakSec = [math]::Round($sw.Elapsed.TotalSeconds,1)
Write-Output ("soak done: {0}s iterations={1} readOK={2} errors={3}" -f $soakSec, $iter, $readOK, $errors.Count)

# kill -9 等效强杀
$killAt = Get-Date
Stop-Process -Id $BridgePid -Force
$gone = $false
$sw2 = [Diagnostics.Stopwatch]::StartNew()
while ($sw2.Elapsed.TotalSeconds -lt $UnmountTimeoutSec) {
    Start-Sleep -Milliseconds 500
    $v = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='$($Drive.TrimEnd('\'))'" -ErrorAction SilentlyContinue
    if (-not $v) { $gone = $true; break }
}
$goneSec = [math]::Round($sw2.Elapsed.TotalSeconds,1)
Write-Output ("after kill: drive gone={0} after {1}s" -f $gone, $goneSec)

$ok = ($errors.Count -eq 0 -and $readOK -eq $iter -and $gone)
$out = [pscustomobject]@{
    tag=$Tag; drive=$Drive; bridgePid=$BridgePid; soakSeconds=$soakSec
    iterations=$iter; readOK=$readOK; errors=$errors; killedAt=$killAt.ToString('HH:mm:ss')
    driveGoneAfterKill=$gone; goneAfterSec=$goneSec; pass=$ok
}
$out | ConvertTo-Json -Depth 4 | Out-File -Encoding utf8 (Join-Path $runsDir "m1-soak-$Tag.json")
$out | Format-List | Out-String | Write-Output
"SUMMARY: soak $($soakSec)s/$Seconds s, $readOK/$iter reads OK, errors=$($errors.Count), drive-gone-after-kill=$gone (${goneSec}s) => $(if($ok){'PASS'}else{'FAIL'})"
if (-not $ok) { exit 1 }

