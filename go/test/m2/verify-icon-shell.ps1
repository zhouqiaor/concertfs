# Verify a mounted PhoneFS drive icon through the real Windows Shell channel.
# Read-only: reads autorun.inf and calls SHGetFileInfo; never writes the registry.
# powershell -NoProfile -ExecutionPolicy Bypass -File test/m2/verify-icon-shell.ps1 -Drive P: -ExpectedIcon C:\abs\drive_phone.ico -Tag phone
param(
    [ValidatePattern('^[A-Za-z]:$')]
    [string]$Drive = 'P:',
    [Parameter(Mandatory=$true)][string]$ExpectedIcon,
    [string]$Tag = 'icon'
)
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [Text.Encoding]::UTF8

$src = @'
using System;
using System.Runtime.InteropServices;
public static class PhoneFSShellIcon {
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
    public struct SHFILEINFO {
        public IntPtr hIcon;
        public int iIcon;
        public uint dwAttributes;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst=260)] public string szDisplayName;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst=80)] public string szTypeName;
    }
    [DllImport("shell32.dll", CharSet=CharSet.Unicode)]
    public static extern IntPtr SHGetFileInfo(string path, uint attrs, ref SHFILEINFO info, uint size, uint flags);
    [DllImport("user32.dll")]
    public static extern bool DestroyIcon(IntPtr hIcon);
}
'@
Add-Type -TypeDefinition $src -Language CSharp
Add-Type -AssemblyName System.Drawing

$runsDir = Join-Path $PSScriptRoot 'runs'
New-Item -ItemType Directory -Force -Path $runsDir | Out-Null
$root = $Drive + '\'
$autorun = Join-Path $root 'autorun.inf'
$expectedFull = [IO.Path]::GetFullPath($ExpectedIcon)
$content = [IO.File]::ReadAllText($autorun, [Text.Encoding]::ASCII)
$wantContent = "[autorun]`r`nIcon=$expectedFull`r`n"
$contentOK = ($content -ceq $wantContent)

# SHGFI_ICONLOCATION: the icon source selected by Explorer's Shell icon loader.
$loc = New-Object PhoneFSShellIcon+SHFILEINFO
$cb = [Runtime.InteropServices.Marshal]::SizeOf($loc)
$locRet = [PhoneFSShellIcon]::SHGetFileInfo($root, 0, [ref]$loc, $cb, 0x1000)
$location = $loc.szDisplayName
$locationOK = ($location -ieq $expectedFull)

# SHGFI_ICON: extract the actual Shell icon handle and save a PNG as evidence.
$info = New-Object PhoneFSShellIcon+SHFILEINFO
$iconRet = [PhoneFSShellIcon]::SHGetFileInfo($root, 0, [ref]$info, $cb, 0x100)
$png = Join-Path $runsDir ("icon-shell-" + $Tag + '.png')
$iconSize = ''
if ($info.hIcon -ne [IntPtr]::Zero) {
    try {
        $ic = [Drawing.Icon]::FromHandle($info.hIcon)
        $bmp = $ic.ToBitmap()
        $bmp.Save($png, [Drawing.Imaging.ImageFormat]::Png)
        $iconSize = "$($bmp.Width)x$($bmp.Height)"
        $bmp.Dispose()
        $ic.Dispose()
    } finally {
        [void][PhoneFSShellIcon]::DestroyIcon($info.hIcon)
    }
}
$pass = $contentOK -and $locationOK -and ($info.hIcon -ne [IntPtr]::Zero)
$result = [pscustomobject]@{
    drive = $Drive
    expected_icon = $expectedFull
    autorun_content = ($content -replace "`r", '\r' -replace "`n", '\n')
    autorun_content_match = $contentOK
    shgfi_iconlocation_ret = $locRet.ToInt64()
    shell_icon_location = $location
    shell_icon_index = $loc.iIcon
    shell_location_matches_expected = $locationOK
    shgfi_icon_ret = $iconRet.ToInt64()
    shell_hicon_nonzero = ($info.hIcon -ne [IntPtr]::Zero)
    extracted_icon_size = $iconSize
    evidence_png = $png
    registry_modified = $false
    pass = $pass
}
$json = Join-Path $runsDir ("icon-shell-" + $Tag + '.json')
$txt = Join-Path $runsDir ("icon-shell-" + $Tag + '.txt')
$result | ConvertTo-Json -Depth 3 | Out-File -Encoding utf8 $json
@(
    "PhoneFS M2 Shell icon verification - $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
    "PASS=$pass drive=$Drive"
    "autorun_content_match=$contentOK content=$($result.autorun_content)"
    "SHGetFileInfo ICONLOCATION='$location' index=$($loc.iIcon) matches_expected=$locationOK"
    "SHGetFileInfo ICON hIcon_nonzero=$($result.shell_hicon_nonzero) size=$iconSize png=$png"
    "registry_modified=False"
) | Out-File -Encoding utf8 $txt
Get-Content $txt
if (-not $pass) { exit 1 }
