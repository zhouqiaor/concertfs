<#
.SYNOPSIS
    PhoneFS Phase 0 codegen 入口（Windows PowerShell 5.1+）。

.DESCRIPTION
    从 contract/openapi/phonefs.yaml 生成：
      - Go models  -> go/internal/gen/phonefs.gen.go            (oapi-codegen 2.8.2)
      - Kotlin     -> android/core-protocol/src/generated/kotlin (openapi-generator 7.26.0)
      - 错误码常量 -> go/internal/fsprovider/error_codes.gen.go
                      android/.../protocol/ErrCodes.gen.kt      (gen_error_codes.py)

    契约本体只读：生成前由 normalize_codegen_spec.py 在 scripts/tools/.codegen 下
    写一份临时副本，去掉 properties 同层的 required-only anyOf 与显式
    additionalProperties: true。
    可重复运行：生成目录每次重建，生成物内容确定性，重复运行零 diff。

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File projects/phonefs/scripts/gen-contract.ps1
#>
[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ProjectDir = Split-Path -Parent $ScriptDir
Set-Location $ProjectDir

$Contract = "contract/openapi/phonefs.yaml"
$GoOut = "go/internal/gen/phonefs.gen.go"
$KtOut = "android/core-protocol/src/generated/kotlin"
$ToolsDir = "scripts/tools"
$NormalizedSpec = "$ToolsDir/.codegen/phonefs.normalized.yaml"

$OapiCodegenVersion = "v2.8.2"
$OpenApiGeneratorVersion = "7.26.0"
$OpenApiGeneratorSha256 = "1760050094997b9cc790cc1350be095f15da8c08a996b184d3eadc4ccda5e35e"
$OpenApiGeneratorJar = "$ToolsDir/openapi-generator-cli-$OpenApiGeneratorVersion.jar"
$OpenApiGeneratorUrl = "https://repo.huaweicloud.com/repository/maven/org/openapitools/openapi-generator-cli/$OpenApiGeneratorVersion/openapi-generator-cli-$OpenApiGeneratorVersion.jar"

# ---------- 工具探测 ----------
$GoBin = $env:GO_BIN
if (-not $GoBin) {
    # 本机构建机 PATH 上的 go 可能是 386；优先显式 amd64 安装。
    if (Test-Path "C:\Programs\go-amd64\bin\go.exe") {
        $GoBin = "C:\Programs\go-amd64\bin\go.exe"
    } else {
        $GoBin = "go"
    }
}

$PythonBin = $env:PYTHON_BIN
if (-not $PythonBin) {
    $PythonBin = (Get-Command python -ErrorAction SilentlyContinue).Source
    if (-not $PythonBin) { $PythonBin = (Get-Command python3 -ErrorAction SilentlyContinue).Source }
}
if (-not $PythonBin) {
    throw "python not found (set PYTHON_BIN)"
}

$JavaBin = $env:JAVA_BIN
if (-not $JavaBin) {
    if ($env:JAVA_HOME -and (Test-Path (Join-Path $env:JAVA_HOME "bin\java.exe"))) {
        $JavaBin = Join-Path $env:JAVA_HOME "bin\java.exe"
    } else {
        $JavaBin = (Get-Command java -ErrorAction SilentlyContinue).Source
    }
}
if (-not $JavaBin) {
    throw "java not found (set JAVA_HOME or JAVA_BIN)"
}

$GoPath = (& $GoBin env GOPATH).Trim()
$OapiBin = $env:OAPI_CODEGEN
if (-not $OapiBin) {
    $candidate = Join-Path $GoPath "bin\oapi-codegen.exe"
    if (Get-Command oapi-codegen -ErrorAction SilentlyContinue) {
        $OapiBin = (Get-Command oapi-codegen).Source
    } elseif (Test-Path $candidate) {
        $OapiBin = $candidate
    } else {
        Write-Host "==> installing oapi-codegen $OapiCodegenVersion"
        & $GoBin install "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$OapiCodegenVersion"
        if ($LASTEXITCODE -ne 0) { throw "go install oapi-codegen failed" }
        $OapiBin = $candidate
    }
}
if (-not (Test-Path $OapiBin)) {
    throw "oapi-codegen not found at $OapiBin"
}

New-Item -ItemType Directory -Force -Path $ToolsDir | Out-Null

# ---------- 下载并校验 openapi-generator ----------
if (-not (Test-Path $OpenApiGeneratorJar)) {
    Write-Host "==> downloading openapi-generator-cli $OpenApiGeneratorVersion"
    $partial = "$OpenApiGeneratorJar.part"
    Invoke-WebRequest -Uri $OpenApiGeneratorUrl -OutFile $partial -UseBasicParsing
    $partialHash = (Get-FileHash -Algorithm SHA256 -Path $partial).Hash.ToLowerInvariant()
    if ($partialHash -ne $OpenApiGeneratorSha256) {
        Remove-Item -Force $partial
        throw "openapi-generator jar sha256 mismatch: $partialHash"
    }
    Move-Item -Force $partial $OpenApiGeneratorJar
}

$actualHash = (Get-FileHash -Algorithm SHA256 -Path $OpenApiGeneratorJar).Hash.ToLowerInvariant()
if ($actualHash -ne $OpenApiGeneratorSha256) {
    Remove-Item -Force $OpenApiGeneratorJar
    throw "openapi-generator jar sha256 mismatch: $actualHash"
}
Write-Host "==> openapi-generator jar sha256 OK ($actualHash)"

# ---------- 1. 规范化临时副本 ----------
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $NormalizedSpec) | Out-Null
& $PythonBin "scripts/normalize_codegen_spec.py" $Contract $NormalizedSpec
if ($LASTEXITCODE -ne 0) { throw "normalize_codegen_spec.py failed" }

# ---------- 2. Go models ----------
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $GoOut) | Out-Null
Write-Host "==> go: oapi-codegen -generate types"
& $OapiBin -generate types -package gen -o $GoOut $NormalizedSpec
if ($LASTEXITCODE -ne 0) { throw "oapi-codegen failed" }

# ---------- 3. Kotlin models ----------
if (Test-Path $KtOut) { Remove-Item -Recurse -Force $KtOut }
Write-Host "==> kotlin: openapi-generator $OpenApiGeneratorVersion (models only)"
& $JavaBin -jar $OpenApiGeneratorJar generate `
    -i $NormalizedSpec `
    -g kotlin `
    --type-mappings="object=JsonObject" `
    --import-mappings="JsonObject=kotlinx.serialization.json.JsonObject" `
    --additional-properties="serializationLibrary=kotlinx_serialization,enumPropertyNaming=UPPERCASE,modelPackage=com.phonefs.core.protocol.gen,sourceFolder=,openApiNullable=false" `
    --global-property="models,modelDocs=false,modelTests=false,supportingFiles=false" `
    -o $KtOut *> "$ToolsDir/kotlin-generator.log"
if ($LASTEXITCODE -ne 0) {
    Get-Content "$ToolsDir/kotlin-generator.log" | Write-Error
    throw "openapi-generator failed"
}
# 生成器元数据与空脚手架目录不随源码提交
Remove-Item -Recurse -Force "$KtOut/.openapi-generator" -ErrorAction SilentlyContinue
Get-ChildItem $KtOut -Directory -Recurse -Force |
    Sort-Object FullName -Descending |
    Where-Object { -not (Get-ChildItem $_.FullName -Force) } |
    Remove-Item -Force

# ---------- 4. 错误码常量 ----------
& $PythonBin "scripts/gen_error_codes.py"
if ($LASTEXITCODE -ne 0) { throw "gen_error_codes.py failed" }

# ---------- 汇总 ----------
$goLines = [System.IO.File]::ReadAllLines((Resolve-Path $GoOut)).Count
$ktFiles = (Get-ChildItem $KtOut -Filter *.kt -Recurse | Measure-Object).Count
Write-Host "==> generated:"
Write-Host "    $GoOut ($goLines lines)"
Write-Host "    $KtOut ($ktFiles files)"
Write-Host "    go/internal/fsprovider/error_codes.gen.go"
Write-Host "    android/core-protocol/src/main/kotlin/com/phonefs/core/protocol/ErrCodes.gen.kt"

Remove-Item -Force $NormalizedSpec -ErrorAction SilentlyContinue
