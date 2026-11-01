# PhoneFS M1 真机会话用：W2 服务端已知会被系统回收（W2 在查），本脚本每 3 秒探活
# /v1/pair/info，发现连接失败就重启 SharingService(FGS)，保证桥侧测试有稳定后端。
# 用法（Git Bash）:
#   powershell -NoProfile -ExecutionPolicy Bypass -File test/m1/keepalive-phonefs.ps1 -Minutes 8
# 只对本会话设备操作，不碰主仓任何文件；stdout 即证据。
param(
    [int]$Minutes = 8,
    [string]$Serial = '192.168.43.1:5555',
    [string]$BaseUrl = 'https://192.168.43.1:18443',
    [string]$Adb = 'C:/Programs/Android/Sdk/platform-tools/adb.exe'
)
$ErrorActionPreference = 'Stop'
# 强制 PATH 透传按字面给 adb，避免 Git Bash 转换 Android 路径
$env:MSYS_NO_PATHCONV = '1'
$deadline = (Get-Date).AddMinutes($Minutes)
$restarts = 0
$sw = [Diagnostics.Stopwatch]::StartNew()
function Ping-Server {
    try {
        & curl.exe -sk --max-time 3 -o NUL -w '%{http_code}' ($BaseUrl + '/v1/pair/info') 2>$null
    } catch { '000' }
}
Write-Output ("keepalive start {0} device={1}" -f (Get-Date -Format 'HH:mm:ss'), $Serial)
while ((Get-Date) -lt $deadline) {
    $code = (Ping-Server).Trim()
    if ($code -ne '200') {
        $restarts++
        Write-Output ("[{0}] pair={1}; restarting SharingService (#{2})" -f (Get-Date -Format 'HH:mm:ss'), $code, $restarts)
        & $Adb -s $Serial shell am start-foreground-service -n com.phonefs.source/.SharingService | Out-Null
        Start-Sleep -Seconds 2
    }
    Start-Sleep -Seconds 3
}
Write-Output ("keepalive done: {0}s, restarts={1}, last pair={2}" -f [math]::Round($sw.Elapsed.TotalSeconds), $restarts, (Ping-Server).Trim())

