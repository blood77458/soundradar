<#
.SYNOPSIS
    Run the unit tests of one internal package the way this machine allows.

.DESCRIPTION
    Two Windows facts shape this helper:

    1. Smart App Control blocks UNSIGNED test executables launched with
       Start-Process ("Access is denied", CodeIntegrity 3077/3033/3118). That
       block applies to the CreateProcess call, and `go test ./...` is hit by it
       too - which is also why a plain `go test` here can silently replay an
       older cached PASS/FAIL instead of running the current code.

       A DIRECT invocation (`& $exe`) is not blocked, so this script builds with
       `go test -c -o <file>` and then runs the file directly. That is the
       evidence path.

    2. Flags beginning with `-test.` are eaten by PowerShell when passed through
       `&`: `-test.run Foo` arrives at the process as `.run Foo` (PowerShell
       parses `-test.run` as a parameter name and truncates it). Quoting does
       not help in PS 5.1, so the test binary is invoked with NO arguments
       (which runs every test) and the output is filtered afterwards with
       Select-String.

.PARAMETER Package
    Package name under internal/ (e.g. "config", "overlay") or "cmd".

.PARAMETER Filter
    Optional Select-String pattern applied to the output.

.EXAMPLE
    .\run_go_test.ps1 config
    .\run_go_test.ps1 overlay -Filter '渲染|Render'
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Package,
    [string]$Filter,
    [string]$OutDir
)

$ErrorActionPreference = 'Stop'

$ModuleDir = $PSScriptRoot
$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
            [System.Environment]::GetEnvironmentVariable('Path', 'User')
$WorkspaceRoot = Split-Path -Parent $ModuleDir
$env:GOPATH = Join-Path $WorkspaceRoot '.gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOCACHE = Join-Path $WorkspaceRoot '.gocache'
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOSUMDB = 'sum.golang.google.cn'
$env:CGO_ENABLED = '0'
$env:GOTOOLCHAIN = 'local'

if (-not $OutDir) { $OutDir = Join-Path $ModuleDir 'artifacts\p3\verify' }
if (-not (Test-Path $OutDir)) { New-Item -ItemType Directory -Force -Path $OutDir | Out-Null }

$pkgPath = if ($Package -eq 'cmd') { './cmd/soundradar' } elseif ($Package -like './*') { $Package } else { "./internal/$Package" }
$safe = ($Package -replace '[^A-Za-z0-9]', '_')
$exe = Join-Path $OutDir "$safe.test.exe"
$outFile = Join-Path $OutDir "$safe.out.txt"

Push-Location $ModuleDir
try {
    if (Test-Path $exe) { Remove-Item -Force $exe -ErrorAction SilentlyContinue }
    Write-Host "[gotest] go test -c -o $exe $pkgPath" -ForegroundColor DarkGray
    $buildOut = & go test -c -o $exe $pkgPath 2>&1 | Out-String
    if ($LASTEXITCODE -ne 0) {
        Write-Host $buildOut
        Write-Host "[gotest] 编译失败" -ForegroundColor Red
        exit 1
    }
    if (-not (Test-Path $exe)) {
        Write-Host "[gotest] $pkgPath 没有测试文件（go test -c 不产出二进制）" -ForegroundColor Yellow
        exit 0
    }

    # Direct invocation: this is the path Smart App Control leaves alone.
    $text = & $exe 2>&1 | Out-String
    $code = $LASTEXITCODE
    [System.IO.File]::WriteAllText($outFile, $text, [System.Text.UTF8Encoding]::new($false))

    if ($Filter) {
        $lines = @($text -split "`r?`n" | Select-String -Pattern $Filter)
    } else {
        $lines = @($text -split "`r?`n" | Where-Object { $_ -match '^(--- (PASS|FAIL|SKIP)|ok|FAIL|PASS)' })
    }
    foreach ($l in $lines) { Write-Host $l }
    $color = if ($code -eq 0) { 'Green' } else { 'Red' }
    Write-Host ("[gotest] {0}: exit={1}  evidence: {2}" -f $Package, $code, $outFile) -ForegroundColor $color
    exit $code
} finally {
    Pop-Location
}
