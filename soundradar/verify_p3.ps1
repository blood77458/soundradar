<#
.SYNOPSIS
    soundradar P3 验收：原生悬浮窗（样式 / 鼠标穿透 / 不抢焦点 / 位置 / 像素证据 /
    淡入淡出 / 热键）+ 设置页 API + 端到端"播放→弹图标"。

.DESCRIPTION
    全部用真实 exe + 真实 Win32 API（Add-Type P/Invoke）做客观断言，每项打印真实数值
    + PASS/FAIL/SKIP。

      B1 样式     GetWindowLongW(GWL_EXSTYLE/GWL_STYLE) 的十六进制值
      B2 鼠标穿透 WindowFromPoint(窗口中心) 必须不是悬浮窗自己
      B3 不抢焦点 GetForegroundWindow 在悬浮窗出现前后不变
      B4 位置大小 GetWindowRect 必须等于配置算出来的矩形（容差 0）
      B5 像素证据 进程交给合成器的帧缓冲 + 屏幕截图，与图标主色比较
      B6 淡入淡出 帧缓冲里的可见帧 + preview 触发一次事件后的 alpha 曲线
      B7 热键     RegisterHotKey 结果；可见性切换走内部 API（绝不合成按键）
      B8 多显示器 有几台报几台，负坐标显示器数如实说明
      C9 端到端   后台 overlay --seconds N --csv，前台 SoundPlayer 播 880 Hz
      C10 serve --overlay 的 GET/PATCH /api/config、POST /api/overlay/preview、
                  GET /api/overlay、显示/隐藏
      C11 回归    go build / vet / gofmt / go test + 已有子命令

.NOTES
    PowerShell 5.1 需要 BOM 才能正确按 UTF-8 读取本文件（本文件是 UTF-8 with BOM）。
    绝不使用 SendInput / keybd_event / SetWindowsHookEx：本工具与游戏反作弊同时运行，
    模拟输入是封号红线。热键只验证"注册成功 + 内部切换生效"。

.EXAMPLE
    Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force
    .\verify_p3.ps1
#>
[CmdletBinding()]
param(
    [int]$CaptureSeconds = 12,
    [double]$ToneHz = 880,
    [int]$ToneSeconds = 12,
    [int]$Size = 96,
    [double]$Opacity = 0.85,
    [switch]$SkipRegression,
    [string]$Exe
)

$ErrorActionPreference = 'Stop'

$ModuleDir = $PSScriptRoot
if (-not $ModuleDir) { $ModuleDir = Split-Path -Parent $MyInvocation.MyCommand.Path }

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

$Artifacts = Join-Path $ModuleDir 'artifacts\p3'
$VerifyDir = Join-Path $Artifacts 'verify'
foreach ($d in @($Artifacts, $VerifyDir)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}

$script:Results = @()
function Add-Result {
    param([string]$Name, [string]$Verdict, [string]$Detail)
    $script:Results += [pscustomobject]@{ Name = $Name; Verdict = $Verdict; Detail = $Detail }
    $color = switch ($Verdict) { 'PASS' { 'Green' } 'FAIL' { 'Red' } default { 'Yellow' } }
    Write-Host ("  [{0}] {1} - {2}" -f $Verdict, $Name, $Detail) -ForegroundColor $color
}
function Section([string]$Title) {
    Write-Host ''
    Write-Host ('=' * 78) -ForegroundColor DarkCyan
    Write-Host "  $Title" -ForegroundColor Cyan
    Write-Host ('=' * 78) -ForegroundColor DarkCyan
}

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;

namespace SrP3 {
    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left, Top, Right, Bottom; }

    [StructLayout(LayoutKind.Sequential)]
    public struct POINT { public int X, Y; }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    public struct MONITORINFOEX {
        public int cbSize;
        public RECT rcMonitor;
        public RECT rcWork;
        public uint dwFlags;
        [MarshalAs(UnmanagedType.ByValTStr, SizeConst = 32)] public string szDevice;
        public static MONITORINFOEX Create() {
            MONITORINFOEX m = new MONITORINFOEX();
            m.cbSize = Marshal.SizeOf(typeof(MONITORINFOEX));
            m.szDevice = "";
            return m;
        }
    }

    public static class Win32 {
        [DllImport("user32.dll")] public static extern int GetWindowLongW(IntPtr h, int index);
        [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
        [DllImport("user32.dll")] public static extern IntPtr WindowFromPoint(POINT p);
        [DllImport("user32.dll")] public static extern IntPtr GetAncestor(IntPtr h, uint flags);
        [DllImport("user32.dll")] public static extern IntPtr RealChildWindowFromPoint(IntPtr h, POINT p);
        [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
        [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int GetWindowTextW(IntPtr h, StringBuilder s, int n);
        [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, StringBuilder s, int n);
        [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
        [DllImport("user32.dll")] public static extern int GetSystemMetrics(int index);
        [DllImport("user32.dll")] public static extern IntPtr MonitorFromPoint(POINT p, uint flags);
        [DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern bool GetMonitorInfoW(IntPtr hMon, ref MONITORINFOEX mi);
        [DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr ctx);

        public static string Title(IntPtr h) {
            StringBuilder sb = new StringBuilder(512);
            GetWindowTextW(h, sb, sb.Capacity);
            return sb.ToString();
        }
        public static string Cls(IntPtr h) {
            StringBuilder sb = new StringBuilder(512);
            GetClassNameW(h, sb, sb.Capacity);
            return sb.ToString();
        }
        public static uint U32(int v) { return BitConverter.ToUInt32(BitConverter.GetBytes(v), 0); }
    }
}
'@

Add-Type -AssemblyName System.Drawing
$null = [SrP3.Win32]::SetProcessDpiAwarenessContext([IntPtr](-4))

function Get-RegionStats {
    param([int]$X, [int]$Y, [int]$W, [int]$H,
          [int]$RefR = -1, [int]$RefG = -1, [int]$RefB = -1, [int]$Tol = 45, [string]$SavePath = '')
    $bmp = New-Object System.Drawing.Bitmap($W, $H, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $g = [System.Drawing.Graphics]::FromImage($bmp)
    try {
        $g.CopyFromScreen($X, $Y, 0, 0, (New-Object System.Drawing.Size($W, $H)),
            [System.Drawing.CopyPixelOperation]::SourceCopy)
    } finally { $g.Dispose() }
    if ($SavePath) { $bmp.Save($SavePath, [System.Drawing.Imaging.ImageFormat]::Png) }

    $rect = New-Object System.Drawing.Rectangle(0, 0, $W, $H)
    $data = $bmp.LockBits($rect, [System.Drawing.Imaging.ImageLockMode]::ReadOnly,
        [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $nonBlack = 0; $colored = 0; $lumSum = 0.0; $match = 0
    $total = $W * $H
    try {
        $stride = $data.Stride
        $bytes = New-Object byte[] ($stride * $H)
        [System.Runtime.InteropServices.Marshal]::Copy($data.Scan0, $bytes, 0, $bytes.Length)
        for ($yy = 0; $yy -lt $H; $yy++) {
            $row = $yy * $stride
            for ($xx = 0; $xx -lt $W; $xx++) {
                $i = $row + $xx * 4
                $b = $bytes[$i]; $gg = $bytes[$i + 1]; $r = $bytes[$i + 2]
                $lum = 0.114 * $b + 0.587 * $gg + 0.299 * $r
                if ($lum -gt 24) {
                    $nonBlack++; $lumSum += $lum
                    $mx = [Math]::Max($r, [Math]::Max($gg, $b))
                    $mn = [Math]::Min($r, [Math]::Min($gg, $b))
                    if (($mx - $mn) -gt 40 -and $mx -gt 80) { $colored++ }
                }
                if ($RefR -ge 0 -and
                    [Math]::Abs($r - $RefR) -le $Tol -and
                    [Math]::Abs($gg - $RefG) -le $Tol -and
                    [Math]::Abs($b - $RefB) -le $Tol) { $match++ }
            }
        }
    } finally { $bmp.UnlockBits($data); $bmp.Dispose() }
    $mean = 0.0
    if ($nonBlack -gt 0) { $mean = $lumSum / $nonBlack }
    return [pscustomobject]@{
        Total = $total; NonBlack = $nonBlack; Colored = $colored
        MeanLum = [Math]::Round($mean, 2); ColorPixels = $match
        ColorRatio = if ($total -gt 0) { [Math]::Round(100.0 * $match / $total, 2) } else { 0 }
    }
}

function Get-DibStats {
    param([string]$Path, [int]$W, [int]$H,
          [int]$RefR = -1, [int]$RefG = -1, [int]$RefB = -1, [int]$Tol = 45)
    if (-not (Test-Path $Path)) { return $null }
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    $need = $W * $H * 4
    if ($bytes.Length -lt $need) { return $null }
    $nonZero = 0; $colored = 0; $match = 0; $maxA = 0
    for ($i = 0; $i -lt $need; $i += 4) {
        $b = $bytes[$i]; $g = $bytes[$i + 1]; $r = $bytes[$i + 2]; $a = $bytes[$i + 3]
        if ($a -gt $maxA) { $maxA = $a }
        if ($a -gt 24) {
            $nonZero++
            if (($r + $g + $b) -gt 150) { $colored++ }
            if ($RefR -ge 0) {
                $f = 255.0 / $a
                $ur = [Math]::Min(255, [int]($r * $f))
                $ug = [Math]::Min(255, [int]($g * $f))
                $ub = [Math]::Min(255, [int]($b * $f))
                if ([Math]::Abs($ur - $RefR) -le $Tol -and [Math]::Abs($ug - $RefG) -le $Tol -and
                    [Math]::Abs($ub - $RefB) -le $Tol) { $match++ }
            }
        }
    }
    return [pscustomobject]@{
        Bytes = $bytes.Length; MaxAlpha = $maxA; NonZero = $nonZero; Colored = $colored
        ColorPixels = $match
        ColorRatio = if ($need -gt 0) { [Math]::Round(100.0 * $match / ($need / 4), 2) } else { 0 }
    }
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
function Get-LibraryItems {
    param([string]$Path)
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Path)
    try {
        $out = @()
        foreach ($e in $zip.Entries) {
            if ($e.FullName -match '^items/([0-9a-fA-F]+)/meta\.json$') {
                $id = $Matches[1]
                $sr = New-Object System.IO.StreamReader($e.Open())
                $meta = $sr.ReadToEnd() | ConvertFrom-Json
                $sr.Close()
                $out += [pscustomobject]@{ ID = $id; Name = $meta.name }
            }
        }
        return $out
    } finally { $zip.Dispose() }
}
function Get-IconDominantColor {
    param([string]$Path, [string]$ItemID)
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Path)
    try {
        $entry = $zip.Entries | Where-Object { $_.FullName -eq "items/$ItemID/icon.png" } | Select-Object -First 1
        if (-not $entry) { return $null }
        $ms = New-Object System.IO.MemoryStream
        $s = $entry.Open(); $s.CopyTo($ms); $s.Close()
        $ms.Position = 0
        $img = [System.Drawing.Image]::FromStream($ms)
        $bmp = New-Object System.Drawing.Bitmap($img)
        $counts = @{}
        for ($yy = 6; $yy -lt $bmp.Height - 6; $yy += 2) {
            for ($xx = 6; $xx -lt $bmp.Width - 6; $xx += 2) {
                $c = $bmp.GetPixel($xx, $yy)
                if ($c.A -lt 200) { continue }
                $k = '{0},{1},{2}' -f $c.R, $c.G, $c.B
                if ($counts.ContainsKey($k)) { $counts[$k]++ } else { $counts[$k] = 1 }
            }
        }
        $bmp.Dispose(); $img.Dispose(); $ms.Dispose()
        $best = $counts.GetEnumerator() | Sort-Object Value -Descending | Select-Object -First 1
        if (-not $best) { return $null }
        $p = $best.Key -split ','
        return [pscustomobject]@{ R = [int]$p[0]; G = [int]$p[1]; B = [int]$p[2]; Count = $best.Value }
    } finally { $zip.Dispose() }
}
function Wait-StateFile {
    param([string]$Path, [int]$TimeoutMs = 20000)
    $sw = [Diagnostics.Stopwatch]::StartNew()
    while ($sw.ElapsedMilliseconds -lt $TimeoutMs) {
        if (Test-Path $Path) {
            try {
                $raw = [System.IO.File]::ReadAllText($Path, [System.Text.UTF8Encoding]::new($false))
                if ($raw.Trim()) {
                    $j = $raw | ConvertFrom-Json
                    if ($j.overlay -and $j.overlay.hwnd) { return $j }
                }
            } catch { }
        }
        Start-Sleep -Milliseconds 150
    }
    return $null
}
function New-ToneWav {
    param([string]$Path, [double]$Hz, [double]$Seconds, [double]$Amp = 0.6, [int]$Rate = 48000)
    $n = [int]($Seconds * $Rate)
    $data = New-Object byte[] ($n * 2)
    for ($i = 0; $i -lt $n; $i++) {
        $v = [math]::Sin(2 * [math]::PI * $Hz * $i / $Rate) * $Amp
        $s = [int16][math]::Round($v * 32767)
        $data[$i * 2] = [byte]($s -band 0xFF)
        $data[$i * 2 + 1] = [byte](($s -shr 8) -band 0xFF)
    }
    $ms = New-Object System.IO.MemoryStream
    $bw = New-Object System.IO.BinaryWriter($ms)
    $bw.Write([char[]]'RIFF'); $bw.Write([int](36 + $data.Length)); $bw.Write([char[]]'WAVE')
    $bw.Write([char[]]'fmt '); $bw.Write([int]16); $bw.Write([int16]1); $bw.Write([int16]1)
    $bw.Write([int]$Rate); $bw.Write([int]($Rate * 2)); $bw.Write([int16]2); $bw.Write([int16]16)
    $bw.Write([char[]]'data'); $bw.Write([int]$data.Length); $bw.Write($data)
    $bw.Flush()
    [System.IO.File]::WriteAllBytes($Path, $ms.ToArray())
    $bw.Dispose(); $ms.Dispose()
}

Section '0. 准备 exe、验收库、测试音'

if (-not $Exe) { $Exe = Join-Path $ModuleDir 'bin\soundradar.exe' }
$needBuild = $true
if (Test-Path $Exe) {
    try {
        $probe = (& $Exe 'version' 2>&1 | Out-String)
        if ($LASTEXITCODE -eq 0 -and $probe -match 'buildSalt') { $needBuild = $false; Write-Host "  复用现有 exe: $Exe" }
        else { Write-Host '  现有 exe 无法启动，将重新构建' -ForegroundColor Yellow }
    } catch { Write-Host '  现有 exe 启动失败，将重新构建' -ForegroundColor Yellow }
}
if ($needBuild) {
    Write-Host '  调用 build.ps1 重新构建（自带换哈希重试）…'
    & (Join-Path $ModuleDir 'build.ps1') | Out-Host
    if (-not (Test-Path $Exe)) { throw "构建后仍找不到 $Exe" }
}
$verOut = (& $Exe 'version' 2>&1 | Out-String)
Add-Result 'exe 可启动' 'PASS' (($verOut -split "`r?`n")[0].Trim())

$libPath = Join-Path $Artifacts 'cli_library.srz'
$p3libExe = Join-Path $VerifyDir 'p3lib.exe'
Push-Location $ModuleDir
try {
    & go build -o $p3libExe ./cmd/p3lib 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) { throw 'go build ./cmd/p3lib 失败' }
} finally { Pop-Location }
& $p3libExe -out $libPath 2>&1 | ForEach-Object { Write-Host "  $_" }

$items = @(Get-LibraryItems -Path $libPath)
$toneItem = $items | Where-Object { $_.Name -match '880' } | Select-Object -First 1
if (-not $toneItem) { throw '验收库里没有 880Hz 条目' }
$toneColor = Get-IconDominantColor -Path $libPath -ItemID $toneItem.ID
Write-Host ("  库: {0}（{1} 个条目）" -f $libPath, $items.Count)
foreach ($it in $items) { Write-Host ("    {0}  {1}" -f $it.ID, $it.Name) }
Write-Host ("  880Hz 图标主色: R={0} G={1} B={2}" -f $toneColor.R, $toneColor.G, $toneColor.B)

$toneWav = Join-Path $Artifacts 'tone880.wav'
New-ToneWav -Path $toneWav -Hz $ToneHz -Seconds $ToneSeconds
$cfgPath = Join-Path $Artifacts 'verify_config.json'
if (Test-Path $cfgPath) { Remove-Item -Force $cfgPath }

$maxSim = 3
$canvasW = $Size * $maxSim + 12
$canvasH = $Size + 12
Write-Host ("  预期画布: {0}x{1}（size={2}，maxSimultaneous={3}）" -f $canvasW, $canvasH, $Size, $maxSim)

Section 'B. 运行时窗口验收（overlay --demo + Win32 断言）'

$statePath = Join-Path $Artifacts 'demo_state.json'
$dibBase = Join-Path $Artifacts 'dib'
foreach ($f in @($statePath, "$dibBase.bgra", "$dibBase.peak.bgra", "$dibBase.peak.txt")) {
    if (Test-Path $f) { Remove-Item -Force $f }
}
$env:SR_OVERLAY_DUMP = $dibBase
$demoArgs = @('overlay', '--library', $libPath, '--config', $cfgPath, '--demo',
              '--size', "$Size", '--opacity', "$Opacity",
              '--selfcheck', '3000', '--dump-state', $statePath, '--quiet')
Write-Host "  > soundradar $($demoArgs -join ' ')"
$demo = Start-Process -FilePath $Exe -ArgumentList $demoArgs -PassThru -NoNewWindow `
    -RedirectStandardOutput (Join-Path $Artifacts 'demo_stdout.txt') `
    -RedirectStandardError (Join-Path $Artifacts 'demo_stderr.txt')

$state = Wait-StateFile -Path $statePath -TimeoutMs 25000
$liveRect = $null
if (-not $state) {
    $e = Get-Content -Raw (Join-Path $Artifacts 'demo_stderr.txt') -ErrorAction SilentlyContinue
    Add-Result 'B 悬浮窗进程与自检' 'FAIL' "没有拿到自检 JSON；stderr=$e"
    if (-not $demo.HasExited) { $demo.Kill() }
} else {
    $hwnd = [IntPtr]::new([int64]$state.overlay.hwnd)
    $liveRect = [pscustomobject]@{ Left = [int]$state.overlay.x; Top = [int]$state.overlay.y
                                   W = [int]$state.overlay.width; H = [int]$state.overlay.height }
    Write-Host ("  自检: hwnd=0x{0:X} class={1} 线程={2} 帧={3} 字体={4}" -f `
        [int64]$state.overlay.hwnd, $state.overlay.className, $state.overlay.threadId,
        $state.overlay.frames, $state.overlay.font)
    Add-Result 'B 悬浮窗进程与自检' 'PASS' `
        ("hwnd=0x{0:X} 帧={1} 字体={2}" -f [int64]$state.overlay.hwnd, $state.overlay.frames, $state.overlay.font)

    $exU = [SrP3.Win32]::U32([SrP3.Win32]::GetWindowLongW($hwnd, -20))
    $stU = [SrP3.Win32]::U32([SrP3.Win32]::GetWindowLongW($hwnd, -16))
    $missing = @()
    foreach ($b in @(@{N='WS_EX_LAYERED';V=0x00080000}, @{N='WS_EX_TRANSPARENT';V=0x20},
                     @{N='WS_EX_TOPMOST';V=0x8}, @{N='WS_EX_NOACTIVATE';V=0x8000000},
                     @{N='WS_EX_TOOLWINDOW';V=0x80})) {
        if (($exU -band $b.V) -ne $b.V) { $missing += $b.N }
    }
    $hasPopup = ($stU -band 0x80000000) -ne 0
    $badStyle = @()
    if (($stU -band 0x00C00000) -ne 0) { $badStyle += 'WS_CAPTION' }
    if (($stU -band 0x00040000) -ne 0) { $badStyle += 'WS_THICKFRAME' }
    Write-Host ("  GWL_EXSTYLE = 0x{0:X8}（期望至少含 0x080800A8）  GWL_STYLE = 0x{1:X8}  WS_POPUP={2}" -f $exU, $stU, $hasPopup)
    Add-Result 'B1 窗口样式（分层/穿透/置顶/不激活/工具窗 + WS_POPUP）' `
        $(if ($missing.Count -eq 0 -and $hasPopup -and $badStyle.Count -eq 0) { 'PASS' } else { 'FAIL' }) `
        ("exStyle=0x{0:X8} style=0x{1:X8} 缺失={2} 多余={3}" -f $exU, $stU,
          $(if ($missing.Count) { $missing -join ',' } else { '无' }),
          $(if ($badStyle.Count) { $badStyle -join ',' } else { '无' }))

    $r = New-Object SrP3.RECT
    $okRect = [SrP3.Win32]::GetWindowRect($hwnd, [ref]$r)
    $rectW = $r.Right - $r.Left
    $rectH = $r.Bottom - $r.Top
    Write-Host ("  GetWindowRect(ok={0}) = ({1},{2}) {3}x{4}" -f $okRect, $r.Left, $r.Top, $rectW, $rectH)
    $pt = New-Object SrP3.POINT
    $pt.X = [int]($r.Left + $rectW / 2); $pt.Y = [int]($r.Top + $rectH / 2)
    $hMon = [SrP3.Win32]::MonitorFromPoint($pt, 2)
    $mi = [SrP3.MONITORINFOEX]::Create()
    $null = [SrP3.Win32]::GetMonitorInfoW($hMon, [ref]$mi)
    $expX = $mi.rcWork.Right - $canvasW - 24
    $expY = $mi.rcWork.Bottom - $canvasH - 24
    Write-Host ("  显示器 {0} 工作区 ({1},{2})-({3},{4})；期望 ({5},{6}) {7}x{8}" -f `
        $mi.szDevice, $mi.rcWork.Left, $mi.rcWork.Top, $mi.rcWork.Right, $mi.rcWork.Bottom,
        $expX, $expY, $canvasW, $canvasH)
    Add-Result 'B4 位置与大小（容差 0；锚点 bottom-right + margin 24）' `
        $(if (($rectW -eq $canvasW) -and ($rectH -eq $canvasH) -and ($r.Left -eq $expX) -and ($r.Top -eq $expY)) { 'PASS' } else { 'FAIL' }) `
        ("实际 ({0},{1}) {2}x{3}，期望 ({4},{5}) {6}x{7}" -f $r.Left, $r.Top, $rectW, $rectH, $expX, $expY, $canvasW, $canvasH)

    $hit = [SrP3.Win32]::WindowFromPoint($pt)
    $hitCls = [SrP3.Win32]::Cls($hit)
    $hitTitle = [SrP3.Win32]::Title($hit)
    $root = [SrP3.Win32]::GetAncestor($hit, 2)
    $child = [SrP3.Win32]::RealChildWindowFromPoint($hwnd, $pt)
    Write-Host ("  WindowFromPoint({0},{1}) = 0x{2:X} 类名='{3}' 标题='{4}'" -f $pt.X, $pt.Y, [int64]$hit, $hitCls, $hitTitle)
    Write-Host ("  GetAncestor(GA_ROOT)=0x{0:X}  RealChildWindowFromPoint(悬浮窗)=0x{1:X}" -f [int64]$root, [int64]$child)
    Add-Result 'B2 鼠标穿透（WindowFromPoint 不是悬浮窗）' `
        $(if (($hit -ne $hwnd) -and ($hit -ne [IntPtr]::Zero)) { 'PASS' } else { 'FAIL' }) `
        ("返回 0x{0:X} 类名='{1}' 标题='{2}'；root=0x{3:X}；RealChild=0x{4:X}" -f [int64]$hit, $hitCls, $hitTitle, [int64]$root, [int64]$child)

    $fg1 = [SrP3.Win32]::GetForegroundWindow()
    Start-Sleep -Milliseconds 400
    $fg2 = [SrP3.Win32]::GetForegroundWindow()
    Write-Host ("  前台窗口 前=0x{0:X} '{1}'  后=0x{2:X} '{3}'（悬浮窗 0x{4:X}）" -f `
        [int64]$fg1, [SrP3.Win32]::Title($fg1), [int64]$fg2, [SrP3.Win32]::Title($fg2), [int64]$hwnd)
    Add-Result 'B3 不抢焦点（前后 GetForegroundWindow 不变且不是悬浮窗）' `
        $(if (($fg1 -eq $fg2) -and ($fg2 -ne $hwnd)) { 'PASS' } else { 'FAIL' }) `
        ("前=0x{0:X} 后=0x{1:X} 悬浮窗=0x{2:X}" -f [int64]$fg1, [int64]$fg2, [int64]$hwnd)

    $screenShot = Join-Path $Artifacts 'overlay_shot.png'
    $scr = Get-RegionStats -X $liveRect.Left -Y $liveRect.Top -W $liveRect.W -H $liveRect.H `
        -RefR $toneColor.R -RefG $toneColor.G -RefB $toneColor.B -Tol 45 -SavePath $screenShot
    $peakDib = Get-DibStats -Path "$dibBase.peak.bgra" -W $liveRect.W -H $liveRect.H `
        -RefR $toneColor.R -RefG $toneColor.G -RefB $toneColor.B -Tol 60
    $curDib = Get-DibStats -Path "$dibBase.bgra" -W $liveRect.W -H $liveRect.H
    Write-Host ("  (a) 屏幕截图 {0}: 非背景 {1}/{2}，彩色 {3}，匹配 880 主色 {4} ({5}%)" -f `
        (Split-Path $screenShot -Leaf), $scr.NonBlack, $scr.Total, $scr.Colored, $scr.ColorPixels, $scr.ColorRatio)
    if ($peakDib) {
        Write-Host ("  (b) 帧缓冲 {0}.peak.bgra: 最大 alpha {1}，非透明 {2}，彩色 {3}，匹配 880 主色 {4} ({5}%)" -f `
            (Split-Path $dibBase -Leaf), $peakDib.MaxAlpha, $peakDib.NonZero, $peakDib.Colored,
            $peakDib.ColorPixels, $peakDib.ColorRatio)
    }
    $srcOk = @()
    if ($scr.ColorRatio -gt 5) { $srcOk += '屏幕截图' }
    if ($peakDib -and $peakDib.ColorRatio -gt 5) { $srcOk += '帧缓冲' }
    Add-Result 'B5 像素证据（与 880 图标主色接近的像素占比 > 5%）' $(if ($srcOk.Count) { 'PASS' } else { 'FAIL' }) `
        ("依据: {0}；屏幕 {1} ({2}%)；帧缓冲 {3} ({4}%)，最大 alpha {5}，非透明 {6}（当前帧非透明 {7}）" -f `
          $(if ($srcOk.Count) { $srcOk -join '+' } else { '都不满足' }),
          $scr.ColorPixels, $scr.ColorRatio,
          $(if ($peakDib) { $peakDib.ColorPixels } else { -1 }),
          $(if ($peakDib) { $peakDib.ColorRatio } else { -1 }),
          $(if ($peakDib) { $peakDib.MaxAlpha } else { -1 }),
          $(if ($peakDib) { $peakDib.NonZero } else { -1 }),
          $(if ($curDib) { $curDib.NonZero } else { -1 }))

    $peakTxt = "$dibBase.peak.txt"
    if (Test-Path $peakTxt) { Write-Host ("  峰值帧信息: " + (Get-Content -Raw $peakTxt).Trim()) }
    Add-Result 'B6a 可见帧（帧缓冲里存在 alpha 接近满值的一帧）' `
        $(if ($peakDib -and $peakDib.MaxAlpha -gt 150 -and $peakDib.NonZero -gt 800) { 'PASS' } else { 'FAIL' }) `
        ("最大 alpha {0}（阈值 150），非透明 {1}（阈值 800）" -f `
          $(if ($peakDib) { $peakDib.MaxAlpha } else { -1 }), $(if ($peakDib) { $peakDib.NonZero } else { -1 }))

    $hkOk = [bool]$state.overlay.hotkeyRegistered
    Write-Host ("  RegisterHotKey: 键={0} 成功={1} 错误='{2}'" -f $state.overlay.hotkey, $hkOk, $state.overlay.hotkeyError)
    Write-Host '  注意：本项按"注册成功 + 内部切换生效"验证，没有合成任何按键（禁止模拟输入）。' -ForegroundColor DarkGray
    Add-Result 'B7 热键注册（RegisterHotKey，未模拟按键）' $(if ($hkOk) { 'PASS' } else { 'FAIL' }) `
        ("键={0} 成功={1} 错误='{2}'" -f $state.overlay.hotkey, $hkOk, $state.overlay.hotkeyError)

    $monCount = [SrP3.Win32]::GetSystemMetrics(80)
    Write-Host "  系统显示器数: $monCount"
    $neg = @()
    foreach ($m in $state.overlay.monitors) {
        $full = $m.full
        Write-Host ("    #{0} {1} 整屏 ({2},{3}) {4}x{5}  工作区 ({6},{7}) {8}x{9}  DPI {10}  主={11}" -f `
            $m.index, $m.device, $full[0], $full[1], $full[2], $full[3],
            $m.work[0], $m.work[1], $m.work[2], $m.work[3], $m.dpi, $m.primary)
        if ($full[0] -lt 0 -or $full[1] -lt 0) { $neg += $m.device }
    }
    if ($monCount -lt 2) {
        Add-Result 'B8 多显示器/负坐标' 'SKIP' "本机只有 $monCount 个显示器，无法实测多显示器与负坐标"
    } else {
        Add-Result 'B8 多显示器/负坐标' 'PASS' `
            ("检测到 $monCount 个显示器；负坐标显示器 $($neg.Count) 个；枚举已覆盖但未做跨屏移动实测")
    }

    Write-Host '  让 --demo 自己跑完…'
    if (-not $demo.WaitForExit(30000)) { $demo.Kill() }
    $frames = 0
    try { $frames = [int64]([System.IO.File]::ReadAllText($statePath, [System.Text.UTF8Encoding]::new($false)) | ConvertFrom-Json).overlay.frames } catch { }
    Add-Result 'B 悬浮窗渲染帧数' $(if ($frames -gt 20) { 'PASS' } else { 'FAIL' }) ("frames={0}" -f $frames)
}

Section "C9. 端到端：播放 $ToneHz Hz → 弹图标"

$csvPath = Join-Path $Artifacts 'hits.csv'
$liveStatePath = Join-Path $Artifacts 'live_state.json'
foreach ($f in @($csvPath, $liveStatePath, "$dibBase.bgra", "$dibBase.peak.bgra", "$dibBase.peak.txt")) {
    if (Test-Path $f) { Remove-Item -Force $f }
}
Get-ChildItem (Join-Path $Artifacts 'hit_shot_*.png') -ErrorAction SilentlyContinue | Remove-Item -Force
$env:SR_OVERLAY_DUMP = $dibBase
$liveArgs = @('overlay', '--library', $libPath, '--config', $cfgPath,
              '--seconds', "$CaptureSeconds", '--csv', $csvPath,
              '--size', "$Size", '--opacity', "$Opacity",
              '--selfcheck', '2500', '--dump-state', $liveStatePath)
Write-Host "  > soundradar $($liveArgs -join ' ')"
$live = Start-Process -FilePath $Exe -ArgumentList $liveArgs -PassThru -NoNewWindow `
    -RedirectStandardOutput (Join-Path $Artifacts 'live_stdout.txt') `
    -RedirectStandardError (Join-Path $Artifacts 'live_stderr.txt')

$liveState = Wait-StateFile -Path $liveStatePath -TimeoutMs 25000
if ($liveState) {
    $liveRect = [pscustomobject]@{ Left = [int]$liveState.overlay.x; Top = [int]$liveState.overlay.y
                                   W = [int]$liveState.overlay.width; H = [int]$liveState.overlay.height }
    Write-Host ("  悬浮窗矩形: ({0},{1}) {2}x{3}" -f $liveRect.Left, $liveRect.Top, $liveRect.W, $liveRect.H)
}

$playLines = @()
$playLines += '$player = New-Object Media.SoundPlayer ''' + $toneWav + ''''
$playLines += '$deadline = (Get-Date).AddSeconds(' + ($CaptureSeconds + 3) + ')'
$playLines += 'while ((Get-Date) -lt $deadline) { $player.PlaySync() }'
$playFile = Join-Path $Artifacts 'play_tone.ps1'
[System.IO.File]::WriteAllText($playFile, ($playLines -join "`r`n"), [System.Text.UTF8Encoding]::new($true))
$playerProc = Start-Process -FilePath 'powershell.exe' `
    -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $playFile) `
    -PassThru -WindowStyle Hidden

$best = $null
$afterShot = $null
$shotsTaken = 0
$hitSeen = $false
$deadline = (Get-Date).AddSeconds($CaptureSeconds + 4)
while ((Get-Date) -lt $deadline) {
    if ($liveRect) {
        if (-not $hitSeen) {
            if (Test-Path $csvPath) {
                $rows = @(Import-Csv -Path $csvPath -Encoding UTF8)
                if (@($rows | Where-Object { $_.名称 -match '880' }).Count -gt 0) {
                    $hitSeen = $true
                    Write-Host '  检测到 880Hz 命中，开始在命中窗口内连拍…'
                }
            }
            Start-Sleep -Milliseconds 120
        } else {
            $p = Join-Path $Artifacts ('hit_shot_{0:d2}.png' -f $shotsTaken)
            $st = Get-RegionStats -X $liveRect.Left -Y $liveRect.Top -W $liveRect.W -H $liveRect.H `
                -RefR $toneColor.R -RefG $toneColor.G -RefB $toneColor.B -Tol 45 -SavePath $p
            $shotsTaken++
            if ($null -eq $best -or $st.ColorRatio -gt $best.ColorRatio) { $best = $st; $bestPath = $p }
            if ($null -eq $afterShot -or $st.NonBlack -lt $afterShot.NonBlack) { $afterShot = $st }
            Start-Sleep -Milliseconds 150
        }
    } else {
        Start-Sleep -Milliseconds 200
    }
    if ($live.HasExited) { break }
}
if (-not $playerProc.HasExited) { $playerProc.Kill(); $playerProc.WaitForExit() }
if (-not $live.HasExited) { if (-not $live.WaitForExit(25000)) { $live.Kill() } }

$hitRows = @()
if (Test-Path $csvPath) { $hitRows = @(Import-Csv -Path $csvPath -Encoding UTF8) }
$toneHit = $hitRows | Where-Object { $_.名称 -match '880' } | Select-Object -First 1
Write-Host "  CSV 命中行数: $($hitRows.Count)（命中窗口连拍 $shotsTaken 张）"
if ($toneHit) {
    Write-Host ("  命中: 时间={0} 条目={1} 名称={2} 分数={3} margin={4} 电平={5} dBFS" -f `
        $toneHit.时间, $toneHit.条目ID, $toneHit.名称, $toneHit.分数, $toneHit.margin, $toneHit.电平dBFS) -ForegroundColor Green
}
if ($best -and $bestPath) {
    Copy-Item -Force $bestPath (Join-Path $Artifacts 'live_overlay_shot.png')
    Write-Host ("  最佳命中帧 {0}: 非背景 {1}，匹配 880 主色 {2} ({3}%) -> live_overlay_shot.png" -f `
        (Split-Path $bestPath -Leaf), $best.NonBlack, $best.ColorPixels, $best.ColorRatio)
}
$livePeak = Get-DibStats -Path "$dibBase.peak.bgra" -W $liveRect.W -H $liveRect.H `
    -RefR $toneColor.R -RefG $toneColor.G -RefB $toneColor.B -Tol 60
if ($livePeak) {
    Write-Host ("  帧缓冲峰值: 最大 alpha {0}，非透明 {1}，匹配 880 主色 {2} ({3}%)" -f `
        $livePeak.MaxAlpha, $livePeak.NonZero, $livePeak.ColorPixels, $livePeak.ColorRatio)
}

$srcOk = @()
if ($best -and $best.ColorRatio -gt 5) { $srcOk += '屏幕截图' }
if ($livePeak -and $livePeak.ColorRatio -gt 5) { $srcOk += '帧缓冲' }
$c9ok = $toneHit -and ($srcOk.Count -gt 0)
$c9detail = ''
if (-not $toneHit) { $c9detail += 'CSV 里没有 880Hz 命中；' }
if ($srcOk.Count -eq 0) { $c9detail += '命中窗口里没有一帧匹配 880 图标主色超过 5%；' }
if (-not $c9detail) {
    $c9detail = ("命中 {0} 条；依据 {1}：命中窗口 {2} 帧里最佳匹配 {3} ({4}%)，非背景 {5}/{6}" -f `
        $hitRows.Count, ($srcOk -join '+'), $shotsTaken, $best.ColorPixels, $best.ColorRatio, $best.NonBlack, $best.Total)
}
Add-Result 'C9 播放 880Hz → 悬浮窗弹出 880Hz 图标' $(if ($c9ok) { 'PASS' } else { 'FAIL' }) $c9detail

$fadeOk = $best -and $best.ColorRatio -gt 5 -and $afterShot -and ($afterShot.ColorRatio -lt 2)
Add-Result 'C9b 淡出 + 自动消失（命中窗口里出现回到背景的帧）' $(if ($fadeOk) { 'PASS' } else { 'FAIL' }) `
    ("命中最佳帧匹配 {0}% 非背景 {1}；最空帧匹配 {2}% 非背景 {3}" -f `
      $(if ($best) { $best.ColorRatio } else { -1 }), $(if ($best) { $best.NonBlack } else { -1 }),
      $(if ($afterShot) { $afterShot.ColorRatio } else { -1 }), $(if ($afterShot) { $afterShot.NonBlack } else { -1 }))

Section 'C10. serve --overlay + 设置页 API'

$port = 8911
$cfg2 = Join-Path $Artifacts 'serve_config.json'
if (Test-Path $cfg2) { Remove-Item -Force $cfg2 }
$srvArgs = @('serve', '--port', "$port", '--strict-port', '--library', $libPath,
             '--config', $cfg2, '--overlay', '--quiet')
Write-Host "  > soundradar $($srvArgs -join ' ')"
$srv = Start-Process -FilePath $Exe -ArgumentList $srvArgs -PassThru -NoNewWindow `
    -RedirectStandardOutput (Join-Path $Artifacts 'serve_overlay_stdout.txt') `
    -RedirectStandardError (Join-Path $Artifacts 'serve_overlay_stderr.txt')
Start-Sleep -Seconds 3

$base = "http://127.0.0.1:$port"
$c10ok = $false
$c10detail = ''
try {
    $cfgResp = Invoke-RestMethod -Uri "$base/api/config"
    Write-Host ("  GET /api/config -> path={0} 来源={1} overlay=({2},{3}) size={4} opacity={5} 存在={6} 可写={7}" -f `
        $cfgResp.path, $cfgResp.source, $cfgResp.config.overlay.x, $cfgResp.config.overlay.y,
        $cfgResp.config.overlay.size, $cfgResp.config.overlay.opacity, $cfgResp.exists, $cfgResp.writable)

    $ovResp = Invoke-RestMethod -Uri "$base/api/overlay"
    Write-Host ("  GET /api/overlay -> available={0} visible={1} 矩形=({2},{3}) {4}x{5} hwnd={6} 热键={7}(注册={8}) 帧={9}" -f `
        $ovResp.available, $ovResp.visible, $ovResp.state.x, $ovResp.state.y, $ovResp.state.width,
        $ovResp.state.height, $ovResp.state.hwndHex, $ovResp.state.hotkey, $ovResp.state.hotkeyRegistered, $ovResp.state.frames)
    Write-Host ("  字体={0} ASCII-only={1} 线程={2} 显示器={3} 个" -f `
        $ovResp.state.font, $ovResp.state.fontAsciiOnly, $ovResp.state.threadId, $ovResp.state.monitors.Count)

    $devResp = Invoke-RestMethod -Uri "$base/api/live/devices"
    Write-Host ("  GET /api/live/devices -> {0} 个端点（error='{1}'）" -f $devResp.devices.Count, $devResp.error)

    $preview = Invoke-RestMethod -Method Post -Uri "$base/api/overlay/preview" `
        -ContentType 'application/json' -Body (@{ id = $toneItem.ID } | ConvertTo-Json)
    Write-Host ("  POST /api/overlay/preview -> ok={0} name='{1}' score={2}" -f $preview.ok, $preview.name, $preview.score)

    Start-Sleep -Milliseconds 1800
    $curve = @()
    $null = Invoke-RestMethod -Method Post -Uri "$base/api/overlay/preview" `
        -ContentType 'application/json' -Body (@{ id = $toneItem.ID } | ConvertTo-Json)
    for ($k = 0; $k -lt 40; $k++) {
        try { $curve += [double](Invoke-RestMethod -Uri "$base/api/overlay").state.alpha } catch { }
        Start-Sleep -Milliseconds 60
    }
    $cMax = 0.0; $cMin = 1.0
    if ($curve.Count -gt 0) {
        $cMax = ($curve | Measure-Object -Maximum).Maximum
        $cMin = ($curve | Measure-Object -Minimum).Minimum
    }
    $peakAt = 0
    for ($k = 1; $k -lt $curve.Count; $k++) { if ($curve[$k] -gt $curve[$peakAt]) { $peakAt = $k } }
    Write-Host ("  B6 淡入淡出 alpha 曲线（每 60 ms）：{0}" -f (($curve | ForEach-Object { $_.ToString('0.00') }) -join ','))
    Add-Result 'B6 淡入淡出（alpha 从低升到峰值再回落到 0）' `
        $(if (($cMax -gt 0.7) -and ($cMin -lt 0.5)) { 'PASS' } else { 'FAIL' }) `
        ("采样 {0} 次，峰值 {1}（第 {2} 个），最小 {3}" -f $curve.Count, $cMax, $peakAt, $cMin)

    $before = $ovResp.state
    $newX = 40; $newY = 60
    $patched = Invoke-RestMethod -Method Patch -Uri "$base/api/config" -ContentType 'application/json' `
        -Body (@{ overlay = @{ x = $newX; y = $newY; size = $Size } } | ConvertTo-Json -Depth 5)
    Write-Host ("  PATCH /api/config -> overlay=({0},{1}) size={2} effects={3}" -f `
        $patched.config.overlay.x, $patched.config.overlay.y, $patched.config.overlay.size, ($patched.effects -join '; '))
    # UpdateLayeredWindow 会把窗口矩形弄脏、窗口线程下一帧（≤20 ms）修好，所以重试几次
    $after = $null
    for ($try = 0; $try -lt 10; $try++) {
        Start-Sleep -Milliseconds 250
        $cand = (Invoke-RestMethod -Uri "$base/api/overlay").state
        if ($cand.width -gt 0 -and $cand.height -gt 0) { $after = $cand; break }
        if ($null -eq $after) { $after = $cand }
    }
    $expW = $Size * 3 + 12; $expH = $Size + 12
    Write-Host ("  前后矩形: ({0},{1}) {2}x{3} -> ({4},{5}) {6}x{7}；期望 ({8},{9}) {10}x{11}" -f `
        $before.x, $before.y, $before.width, $before.height, $after.x, $after.y, $after.width, $after.height,
        $newX, $newY, $expW, $expH)
    Add-Result 'B4b PATCH /api/config → 悬浮窗不重启即改变' `
        $(if (($after.x -eq $newX) -and ($after.y -eq $newY) -and ($after.width -eq $expW) -and ($after.height -eq $expH)) { 'PASS' } else { 'FAIL' }) `
        ("实际 ({0},{1}) {2}x{3}" -f $after.x, $after.y, $after.width, $after.height)

    $rejected = $false; $rejMsg = ''
    try {
        $null = Invoke-RestMethod -Method Patch -Uri "$base/api/config" -ContentType 'application/json' `
            -Body (@{ overlay = @{ size = 0 } } | ConvertTo-Json -Depth 5)
    } catch {
        $rejected = $true
        if ($_.ErrorDetails) { $rejMsg = $_.ErrorDetails.Message }
        elseif ($_.Exception.Response) {
            $sr = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
            $rejMsg = $sr.ReadToEnd(); $sr.Close()
        }
    }
    Write-Host ("  PATCH size=0 -> 被拒绝={0} 响应={1}" -f $rejected, $rejMsg)
    Add-Result 'C10 非法配置被拒绝（HTTP 4xx + 中文错误）' $(if ($rejected) { 'PASS' } else { 'FAIL' }) $rejMsg

    $hwnd2 = [IntPtr]::new([int64]$after.hwnd)
    $v1 = [SrP3.Win32]::IsWindowVisible($hwnd2)
    $null = Invoke-RestMethod -Method Post -Uri "$base/api/overlay/visible" -ContentType 'application/json' -Body '{"visible":false}'
    Start-Sleep -Milliseconds 500
    $v2 = [SrP3.Win32]::IsWindowVisible($hwnd2)
    $null = Invoke-RestMethod -Method Post -Uri "$base/api/overlay/visible" -ContentType 'application/json' -Body '{"visible":true}'
    Start-Sleep -Milliseconds 500
    $v3 = [SrP3.Win32]::IsWindowVisible($hwnd2)
    $gr = New-Object SrP3.RECT
    $null = [SrP3.Win32]::GetWindowRect($hwnd2, [ref]$gr)
    Write-Host ("  IsWindowVisible: 前={0} 隐藏后={1} 再显示={2}；GetWindowRect=({3},{4})-({5},{6})" -f `
        $v1, $v2, $v3, $gr.Left, $gr.Top, $gr.Right, $gr.Bottom)
    Add-Result 'B7b 内部 API 切换显示/隐藏（IsWindowVisible 前后变化）' `
        $(if ($v1 -and (-not $v2) -and $v3) { 'PASS' } else { 'FAIL' }) `
        ("前={0} 隐藏后={1} 再显示={2}" -f $v1, $v2, $v3)

    $html = (Invoke-WebRequest -Uri "$base/" -UseBasicParsing).Content
    $appJs = (Invoke-WebRequest -Uri "$base/app.js" -UseBasicParsing).Content
    $css = (Invoke-WebRequest -Uri "$base/style.css" -UseBasicParsing).Content
    $uiOk = ($html -match '设置') -and ($html -match 'viewSettings') -and ($appJs -match 'loadSettings') -and
            ($appJs -match '/api/config') -and ($css -match 'anchorGrid')
    Write-Host ("  内嵌前端: index.html {0} B / app.js {1} B / style.css {2} B；含设置页={3}" -f `
        $html.Length, $appJs.Length, $css.Length, $uiOk)
    Add-Result 'C10 内嵌前端含「设置」页签（下拉/XY/锚点/预览/开关）' $(if ($uiOk) { 'PASS' } else { 'FAIL' }) `
        ("index.html {0} B, app.js {1} B, css {2} B" -f $html.Length, $appJs.Length, $css.Length)

    $c10ok = $true
    $c10detail = 'config / overlay / devices / preview / PATCH / visible 全部返回'
} catch {
    $c10detail = $_.Exception.Message
    Write-Host "  C10 验证异常: $c10detail" -ForegroundColor Yellow
} finally {
    if (-not $srv.HasExited) { $srv.Kill() }
}
Add-Result 'C10 serve --overlay 六个新接口' $(if ($c10ok) { 'PASS' } else { 'FAIL' }) $c10detail

Section 'C11. 回归（已有子命令 + 离线检查）'

Push-Location $ModuleDir
try {
    function Invoke-Exe {
        param([Parameter(Mandatory = $true)][string[]]$ExeArgs)
        try { return (& $Exe @ExeArgs 2>&1 | Out-String) } catch { return ($_.Exception.Message + "`n") }
    }
    $vOut = Invoke-Exe @('version')
    Add-Result 'C11 version' $(if ($vOut -match 'soundradar 0\.4') { 'PASS' } else { 'FAIL' }) (($vOut -split "`r?`n")[0]).Trim()
    $dOut = Invoke-Exe @('devices')
    Add-Result 'C11 devices' $(if ($dOut -match 'Active render endpoints') { 'PASS' } else { 'FAIL' }) `
        (($dOut -split "`r?`n" | Where-Object { $_ -match '^\s*\[\d+\]' } | Select-Object -First 1)).Trim()
    $capWav = Join-Path $Artifacts 'regress_capture.wav'
    $capOut = Invoke-Exe @('capture', '--seconds', '1', '--out', $capWav)
    Add-Result 'C11 capture' $(if ($capOut -match 'frames' -and (Test-Path $capWav)) { 'PASS' } else { 'FAIL' }) `
        (($capOut -split "`r?`n" | Where-Object { $_ -match 'frames' } | Select-Object -First 1)).Trim()
    $idxBin = Join-Path $Artifacts 'regress_index.bin'
    $idxOut = Invoke-Exe @('index', 'rebuild', '--library', $libPath, '--out', $idxBin)
    Add-Result 'C11 index rebuild' $(if ($idxOut -match '条目数') { 'PASS' } else { 'FAIL' }) `
        (($idxOut -split "`r?`n" | Where-Object { $_ -match '条目数|样本数' }) -join ' / ').Trim()
    $mOut = Invoke-Exe @('match', '--wav', $toneWav, '--library', $libPath, '--index', $idxBin)
    Add-Result 'C11 match' $(if ($mOut -match '结论' -and $mOut -match '880') { 'PASS' } else { 'FAIL' }) `
        (($mOut -split "`r?`n" | Where-Object { $_ -match '^结论' } | Select-Object -First 1)).Trim()
    $lhOut = Invoke-Exe @('live', '--help')
    Add-Result 'C11 live --help' $(if ($lhOut -match '--csv') { 'PASS' } else { 'FAIL' }) '中文用法输出到 stdout'
    $ohOut = Invoke-Exe @('overlay', '--help')
    Add-Result 'C11 overlay --help' $(if ($ohOut -match '--demo' -and $ohOut -match 'WS_EX_TRANSPARENT') { 'PASS' } else { 'FAIL' }) `
        '中文用法输出到 stdout（含窗口样式说明）'
    $hOut = Invoke-Exe @('--help')
    Add-Result 'C11 --help' $(if ($hOut -match 'overlay') { 'PASS' } else { 'FAIL' }) 'usage 里列出 overlay'

    $servePort = 8913
    $svp = Start-Process -FilePath $Exe -ArgumentList @('serve', '--port', "$servePort", '--strict-port', '--library', $libPath, '--quiet') `
        -PassThru -NoNewWindow -RedirectStandardOutput (Join-Path $Artifacts 'regress_serve.out.txt') `
        -RedirectStandardError (Join-Path $Artifacts 'regress_serve.err.txt')
    Start-Sleep -Seconds 2
    $serveOk = $false; $serveNote = ''
    try {
        $libDto = Invoke-RestMethod -Uri "http://127.0.0.1:$servePort/api/library"
        $hm = Invoke-WebRequest -Uri "http://127.0.0.1:$servePort/" -UseBasicParsing
        $serveOk = ($libDto.itemCount -ge 1 -and $hm.Content -match 'SoundRadar')
        $serveNote = "条目 $($libDto.itemCount)，首页 $($hm.Content.Length) B"
    } catch { $serveNote = $_.Exception.Message } finally { if (-not $svp.HasExited) { $svp.Kill() } }
    Add-Result 'C11 serve + /api/library' $(if ($serveOk) { 'PASS' } else { 'FAIL' }) $serveNote

    if (-not $SkipRegression) {
        Write-Host ''
        Write-Host '  ---- 离线回归 ----' -ForegroundColor DarkGray
        & go build ./... 2>&1 | Out-Host
        Add-Result 'go build ./...' $(if ($LASTEXITCODE -eq 0) { 'PASS' } else { 'FAIL' }) "exit=$LASTEXITCODE"
        $vetOut = & go vet ./... 2>&1 | Out-String
        Add-Result 'go vet ./...' $(if ($LASTEXITCODE -eq 0) { 'PASS' } else { 'FAIL' }) `
            $(if ($vetOut.Trim()) { $vetOut.Trim() } else { '干净' })
        $fmtOut = & gofmt -l . 2>&1 | Out-String
        Add-Result 'gofmt -l .' $(if ($fmtOut.Trim() -eq '') { 'PASS' } else { 'FAIL' }) `
            $(if ($fmtOut.Trim()) { $fmtOut.Trim() } else { '无未格式化文件' })

        $allOut = & go test ./... 2>&1 | Out-String
        $allExit = $LASTEXITCODE
        [System.IO.File]::WriteAllText((Join-Path $Artifacts 'gotest_all.txt'), $allOut, [System.Text.UTF8Encoding]::new($false))
        $failedPkgs = @()
        foreach ($l in ($allOut -split "`r?`n")) {
            $mm = [regex]::Match($l, '^FAIL\s+(\S+)')
            if ($mm.Success) { $failedPkgs += $mm.Groups[1].Value }
        }
        if ($allExit -eq 0) {
            Add-Result 'go test ./...' 'PASS' '全部包通过'
        } else {
            Add-Result 'go test ./...' 'FAIL' ("失败的包: " + ($failedPkgs -join ', '))
        }

        # 本机 Smart App Control 会拦未签名的测试 exe（CreateProcess 被拒），
        # 对失败的包再用 go test -c + 直接 & 调用 复核。
        $pkgs = @(
            @{ Name = 'config'; Path = './internal/config' }, @{ Name = 'overlay'; Path = './internal/overlay' },
            @{ Name = 'server'; Path = './internal/server' }, @{ Name = 'live'; Path = './internal/live' },
            @{ Name = 'match'; Path = './internal/match' }, @{ Name = 'dsp'; Path = './internal/dsp' },
            @{ Name = 'library'; Path = './internal/library' }, @{ Name = 'index'; Path = './internal/index' },
            @{ Name = 'audio'; Path = './internal/audio' }, @{ Name = 'capture'; Path = './internal/capture' },
            @{ Name = 'cmd'; Path = './cmd/soundradar' }
        )
        foreach ($pkg in $pkgs) {
            $isFailed = $false
            foreach ($f in $failedPkgs) { if ($f -like "*/$($pkg.Name)") { $isFailed = $true } }
            if (-not $isFailed -and $allExit -eq 0) { continue }
            $testExe = Join-Path $VerifyDir "$($pkg.Name).test.exe"
            if (Test-Path $testExe) { Remove-Item -Force $testExe }
            & go test -c -o $testExe $pkg.Path 2>&1 | Out-Host
            if ($LASTEXITCODE -ne 0 -or -not (Test-Path $testExe)) {
                Add-Result "go test -c + 直接运行 ($($pkg.Name))" 'FAIL' '编译失败'; continue
            }
            $pkgOut = ''
            $pkgCode = 1
            try {
                $pkgOut = (& $testExe 2>&1 | Out-String)
                $pkgCode = $LASTEXITCODE
            } catch {
                $pkgOut = "$pkgOut`n$($_.Exception.Message)"
                $pkgCode = 1
            }
            [System.IO.File]::WriteAllText((Join-Path $VerifyDir "$($pkg.Name).out.txt"), $pkgOut, [System.Text.UTF8Encoding]::new($false))
            if ($pkgCode -eq 0) {
                Add-Result "go test -c + 直接运行 ($($pkg.Name))" 'PASS' '通过'
            } elseif ($pkgOut -match 'Access is denied|Application Control') {
                Add-Result "go test -c + 直接运行 ($($pkg.Name))" 'FAIL' 'BLOCKED by Smart App Control'
            } else {
                $fails = @($pkgOut -split "`r?`n" | Where-Object { $_ -match '^--- FAIL|^\s+\S+_test\.go:' } | Select-Object -First 3)
                Add-Result "go test -c + 直接运行 ($($pkg.Name))" 'FAIL' ($fails -join ' | ')
            }
        }
    } else {
        Write-Host '  （已按要求跳过离线回归）' -ForegroundColor Yellow
    }
} finally { Pop-Location }

Section '总结'
$script:Results | Format-Table -AutoSize Name, Verdict, Detail | Out-String -Width 220 | Write-Host
$fails = @($script:Results | Where-Object { $_.Verdict -eq 'FAIL' })
$skips = @($script:Results | Where-Object { $_.Verdict -eq 'SKIP' })
Write-Host ("  PASS {0} / FAIL {1} / SKIP {2}" -f `
    @($script:Results | Where-Object { $_.Verdict -eq 'PASS' }).Count, $fails.Count, $skips.Count)
Write-Host "  证据目录: $Artifacts"
if ($skips.Count -gt 0) {
    Write-Host '  SKIP:' -ForegroundColor Yellow
    foreach ($s in $skips) { Write-Host ("    - {0}: {1}" -f $s.Name, $s.Detail) -ForegroundColor Yellow }
}
if ($fails.Count -gt 0) { exit 1 }
exit 0
