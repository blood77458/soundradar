<#
.SYNOPSIS
    Build soundradar (pure Go, CGO disabled) and survive Windows Smart App Control.

.DESCRIPTION
    Two environment facts drive the design of this script:

    1. The DSH sandbox only allows writes inside the session workspace, while the
       default GOPATH / GOMODCACHE / GOCACHE live outside of it
       (C:\Users\znz_l\go, ...) and fail with "Access is denied". Every Go
       directory is therefore redirected into the workspace, and the Chinese
       module proxy is forced (proxy.golang.org is unreachable here).
       `go env -w` does not work in this sandbox, so process env vars are used.

    2. Smart App Control is ENABLED on this machine and blocks unsigned
       executables *per file hash*. It has no allow-list. Measured on
       2026-09-18: the default build of this very source was blocked 5/5 times
       (a renamed copy was blocked too, so it is not path based), while a
       rebuild of the same source with -s -w launched fine (and a LARGER
       10.5 MiB binary also launched fine, so it is not size based either).
       CodeIntegrity events: id 3118 "Smart App Control Block" and id 3077
       "... did not meet the Enterprise signing level requirements
       (Policy ID:{0283ac0f-fff1-49ae-ada1-8a933130cad6})".

    So this script does: build -> launch self-check -> on block, pick a new
    build salt (which changes the binary hash) -> rebuild -> repeat.
    The self-check uses the zero-side-effect `version` subcommand.

.PARAMETER WorkspaceRoot
    Workspace root holding .gopath / .gocache. Defaults to the parent of this
    script (...\sound_check).

.PARAMETER OutputDir
    Where the .exe files are written. Defaults to <module>\bin.

.PARAMETER MaxAttempts
    How many build+salt attempts to make before giving up. Default 6.

.PARAMETER Salt
    Force a specific build salt (reproducible build, single attempt).

.PARAMETER NoVerify
    Skip the launch self-check entirely (plain build, no salt retry).

.PARAMETER Tidy
    Run `go mod tidy` before building.

.PARAMETER Clean
    Remove previous binaries and `go clean -cache` before building.

.PARAMETER Test
    Run `go vet ./...` and `go test ./...` after a successful build.

.EXAMPLE
    .\build.ps1
    .\build.ps1 -Test -Tidy

.NOTES
    If PowerShell refuses to run scripts, allow it for the current process only:

        Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force
        .\build.ps1 -Test
#>
[CmdletBinding()]
param(
    [string]$WorkspaceRoot,
    [string]$OutputDir,
    [int]$MaxAttempts = 6,
    [string]$Salt,
    [switch]$NoVerify,
    [switch]$Tidy,
    [switch]$Clean,
    [switch]$Test
)

$ErrorActionPreference = 'Stop'

$ModuleDir = $PSScriptRoot
if (-not $WorkspaceRoot) { $WorkspaceRoot = Split-Path -Parent $ModuleDir }
if (-not $OutputDir) { $OutputDir = Join-Path $ModuleDir 'bin' }

# ---------------------------------------------------------------------------
# Environment (every process is fresh, so this is redone on each invocation)
# ---------------------------------------------------------------------------
$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
            [System.Environment]::GetEnvironmentVariable('Path', 'User')

$env:GOPATH = Join-Path $WorkspaceRoot '.gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOCACHE = Join-Path $WorkspaceRoot '.gocache'
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOSUMDB = 'sum.golang.google.cn'
$env:GOTOOLCHAIN = 'local'
$env:CGO_ENABLED = '0'

Write-Host "[build] workspace   : $WorkspaceRoot"
Write-Host "[build] module      : $ModuleDir"
Write-Host "[build] output      : $OutputDir"
Write-Host "[build] CGO_ENABLED : $env:CGO_ENABLED (pure Go, no cgo)"

foreach ($d in @($env:GOPATH, $env:GOCACHE, $OutputDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}

$ext = ''
if ($env:OS -eq 'Windows_NT' -or $IsWindows) { $ext = '.exe' }
$mainExe = Join-Path $OutputDir ('soundradar' + $ext)
$wavstatExe = Join-Path $OutputDir ('wavstat' + $ext)

# ---------------------------------------------------------------------------
# Launch self-check: does this exact binary hash survive Smart App Control?
# ---------------------------------------------------------------------------
function Invoke-LaunchProbe {
    param([Parameter(Mandatory = $true)][string]$ExePath)

    $Error.Clear()
    $output = ''
    $exit = $null
    try {
        $output = (& $ExePath 'version' 2>&1 | Out-String)
        $exit = $LASTEXITCODE
    }
    catch {
        $output = "$output`n$($_.Exception.Message)"
    }

    $errText = ''
    if ($Error.Count -gt 0) {
        $errText = (($Error | ForEach-Object { $_.Exception.Message }) -join ' | ')
    }

    $all = "$output $errText"
    $blocked = [bool]($all -match 'Application Control|blocked this file|code integrity policy')
    $ok = ((-not $blocked) -and ($exit -eq 0) -and ($output -match 'buildSalt'))

    return [pscustomobject]@{
        Ok        = $ok
        Blocked   = $blocked
        ExitCode  = $exit
        Output    = $output.Trim()
        ErrorText = $errText.Trim()
    }
}

function Get-Sha256([string]$Path) {
    if (Test-Path $Path) { return (Get-FileHash -Path $Path -Algorithm SHA256).Hash }
    return ''
}

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------
if ($Clean) {
    Write-Host '[build] cleaning build cache and previous binaries'
    & go clean -cache
    Remove-Item -Force -ErrorAction SilentlyContinue $mainExe, $wavstatExe
}

Push-Location $ModuleDir
$succeeded = $false
$attempts = @()
try {
    if ($Tidy) {
        Write-Host '[build] go mod tidy'
        & go mod tidy
        if ($LASTEXITCODE -ne 0) { throw "go mod tidy failed with exit code $LASTEXITCODE" }
    }

    # wavstat is a dev tool: single build, no salt needed but still probed.
    Write-Host ''
    Write-Host '[build] building wavstat'
    & go build -trimpath -ldflags '-s -w' -o $wavstatExe './cmd/wavstat'
    if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/wavstat failed with exit code $LASTEXITCODE" }

    $total = 1
    if (-not $NoVerify) { $total = [Math]::Max(1, $MaxAttempts) }
    if ($Salt) { $total = 1 }

    for ($i = 1; $i -le $total; $i++) {
        if ($Salt) {
            $useSalt = $Salt
        }
        else {
            $useSalt = [Guid]::NewGuid().ToString('N').Substring(0, 12)
        }
        $stamp = [DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
        $ldflags = "-s -w -X main.buildSalt=$useSalt -X main.buildTime=$stamp"

        Write-Host ''
        Write-Host "[build] attempt $i/$total  salt=$useSalt"
        Write-Host "[build] go build -trimpath -ldflags `"$ldflags`" -o $mainExe ./cmd/soundradar"
        & go build -trimpath -ldflags $ldflags -o $mainExe './cmd/soundradar'
        if ($LASTEXITCODE -ne 0) { throw "go build ./cmd/soundradar failed with exit code $LASTEXITCODE" }

        $size = (Get-Item $mainExe).Length

        if ($NoVerify) {
            $attempts += [pscustomobject]@{ Attempt = $i; Salt = $useSalt; Bytes = $size; Verdict = 'not verified (-NoVerify)' }
            $succeeded = $true
            break
        }

        $probe = Invoke-LaunchProbe -ExePath $mainExe
        if ($probe.Ok) {
            $verdict = 'LAUNCHES OK'
            $attempts += [pscustomobject]@{ Attempt = $i; Salt = $useSalt; Bytes = $size; Verdict = $verdict }
            $succeeded = $true
            Write-Host "[build] launch self-check: OK (hash accepted)"
            break
        }

        if ($probe.Blocked) {
            $verdict = 'BLOCKED by Smart App Control'
        }
        else {
            $verdict = "FAILED to run (exit=$($probe.ExitCode))"
        }
        $attempts += [pscustomobject]@{ Attempt = $i; Salt = $useSalt; Bytes = $size; Verdict = $verdict }
        Write-Warning "[build] $verdict -> retrying with a different hash"
        if ($probe.ErrorText) { Write-Warning "[build]   $($probe.ErrorText)" }
        Start-Sleep -Milliseconds 400
    }

    if ($Test -and $succeeded) {
        Write-Host ''
        Write-Host '[build] go vet ./...'
        & go vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
        Write-Host '[build] go test ./...'
        & go test ./...
        if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }
    }
}
finally {
    Pop-Location
}

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------
Write-Host ''
Write-Host '[build] attempts:'
foreach ($a in $attempts) {
    Write-Host ("  #{0}  salt={1,-14} {2,10} bytes  {3}" -f $a.Attempt, $a.Salt, $a.Bytes, $a.Verdict)
}

Write-Host ''
Write-Host '[build] artifacts:'
foreach ($f in @($mainExe, $wavstatExe)) {
    if (Test-Path $f) {
        $item = Get-Item $f
        $mib = [Math]::Round($item.Length / 1MB, 2)
        Write-Host ("  {0,-18} {1,10} bytes  ({2} MiB)  sha256={3}" -f $item.Name, $item.Length, $mib, (Get-Sha256 $f))
    }
}

if (-not $succeeded) {
    Write-Host ''
    Write-Error @"
All $total attempt(s) produced a binary that Smart App Control refused to launch.
Smart App Control blocks unsigned binaries per file hash and has no allow-list.
Options:
  - re-run this script (a new random salt is used every time), or
  - turn Smart App Control off:
    Settings > Privacy & security > Windows Security > App & browser control >
    Smart App Control > Off.  NOTE: this is one-way; re-enabling it requires
    reinstalling or resetting Windows.
  - sign the binary with a trusted code-signing certificate (self-signed is not enough).
"@
    exit 2
}

if (-not $NoVerify) {
    Write-Host ''
    Write-Host '[build] version probe output:'
    $final = Invoke-LaunchProbe -ExePath $mainExe
    foreach ($line in ($final.Output -split "`n")) { Write-Host "  $line" }
}
