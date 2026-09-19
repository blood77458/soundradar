<#
.SYNOPSIS
    soundradar P2 后半段（实时识别链路 + 实时打分面板 + live CLI）端到端验收。

.DESCRIPTION
    三段真实链路验证，全部使用真实 exe（不是 go test）：

      A. 文件回放（不依赖声卡）
         用 live --wav 回放 "0.5 s 静音 + 0.5 s 880 Hz + 0.5 s 静音"，
         断言 stdout 与 --csv 里出现 880 Hz 的命中，并打印命中行和分数。

      B. 真实采集链路（依赖"播放设备 = 采集端点"）
         后台启动 `live --seconds N --csv out.csv`，另起进程用
         System.Media.SoundPlayer 播放 880 Hz 测试音，结束后断言 CSV 里
         有该音效的命中行。失败时如实报告（不伪造）。

      C. 实时统计
         解析 live 的结束统计（音频块 / 窗口 / tick / 命中 / 丢帧 /
         单块平均与峰值处理耗时 / 每 20 ms 音频耗时 / 堆内存）以及
         进程 CPU 占用与峰值工作集。

    另外跑一遍离线回归：go build / go vet / gofmt / go test（internal/index
    的测试会被 Smart App Control 拦，用 go test -c + Start-Process 规避）。

.NOTES
    PowerShell 5.1 需要 BOM 才能正确按 UTF-8 读取本文件，所以本文件是 UTF-8 with BOM。

.EXAMPLE
    Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force
    .\verify_live.ps1
    .\verify_live.ps1 -CaptureSeconds 12 -SkipRegression
#>
[CmdletBinding()]
param(
    # 真实采集链路持续多少秒（会在这段时间里循环播放测试音）
    [int]$CaptureSeconds = 10,
    # 测试音频率（Hz）与时长（秒）
    [double]$ToneHz = 880,
    [int]$ToneSeconds = 10,
    # 跳过最后的离线回归（go build/vet/test）
    [switch]$SkipRegression,
    # 复用已有的 exe（否则调用 build.ps1 重新构建）
    [string]$Exe
)

$ErrorActionPreference = 'Stop'

$ModuleDir = $PSScriptRoot
if (-not $ModuleDir) { $ModuleDir = Split-Path -Parent $MyInvocation.MyCommand.Path }

# ---------------------------------------------------------------------------
# 环境（每个进程都是新的，必须重设）
# ---------------------------------------------------------------------------
$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' +
            [System.Environment]::GetEnvironmentVariable('Path', 'User')
$WorkspaceRoot = Split-Path -Parent $ModuleDir
$env:GOPATH = Join-Path $WorkspaceRoot '.gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOCACHE = Join-Path $WorkspaceRoot '.gocache'
$env:GOPROXY = 'https://goproxy.cn,direct'
$env:GOSUMDB = 'sum.golang.google.cn'
$env:CGO_ENABLED = '0'

$Artifacts = Join-Path $ModuleDir 'artifacts\p2live'
if (-not (Test-Path $Artifacts)) { New-Item -ItemType Directory -Force -Path $Artifacts | Out-Null }

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

# ---------------------------------------------------------------------------
# 0. 准备 exe 与测试素材
# ---------------------------------------------------------------------------
Section '0. 准备 exe 与测试素材'

if (-not $Exe) { $Exe = Join-Path $ModuleDir 'bin\soundradar.exe' }
$needBuild = $true
if (Test-Path $Exe) {
    # 先探一次：Smart App Control 会按哈希不稳定地拦截未签名 exe。
    try {
        $probe = (& $Exe 'version' 2>&1 | Out-String)
        if ($LASTEXITCODE -eq 0 -and $probe -match 'buildSalt') {
            $needBuild = $false
            Write-Host "  复用现有 exe: $Exe"
        } else {
            Write-Host "  现有 exe 无法启动，将重新构建" -ForegroundColor Yellow
        }
    } catch {
        Write-Host "  现有 exe 启动失败（$($_.Exception.Message)），将重新构建" -ForegroundColor Yellow
    }
}
if ($needBuild) {
    Write-Host '  调用 build.ps1 重新构建（自带换哈希重试）…'
    & (Join-Path $ModuleDir 'build.ps1') | Out-Host
    if (-not (Test-Path $Exe)) { throw "构建后仍找不到 $Exe" }
}
$verOut = (& $Exe 'version' 2>&1 | Out-String)
Write-Host $verOut.Trim()
Add-Result 'exe 可启动' 'PASS' (($verOut -split "`n")[0].Trim())

# 用 wavstat 或直接算？这里用 .NET 生成测试素材，避免依赖别的东西。
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
    $bw.Write([char[]]'RIFF')
    $bw.Write([int](36 + $data.Length))
    $bw.Write([char[]]'WAVE')
    $bw.Write([char[]]'fmt ')
    $bw.Write([int]16)
    $bw.Write([int16]1)
    $bw.Write([int16]1)
    $bw.Write([int]$Rate)
    $bw.Write([int]($Rate * 2))
    $bw.Write([int16]2)
    $bw.Write([int16]16)
    $bw.Write([char[]]'data')
    $bw.Write([int]$data.Length)
    $bw.Write($data)
    $bw.Flush()
    [System.IO.File]::WriteAllBytes($Path, $ms.ToArray())
    $bw.Dispose(); $ms.Dispose()
}

function New-ReplayWav {
    param([string]$Path, [double]$LeadS, [double]$ToneS, [double]$TailS, [double]$Hz, [int]$Rate = 48000)
    $n = [int](($LeadS + $ToneS + $TailS) * $Rate)
    $start = [int]($LeadS * $Rate)
    $end = $start + [int]($ToneS * $Rate)
    $data = New-Object byte[] ($n * 2)
    for ($i = $start; $i -lt $end -and $i -lt $n; $i++) {
        $v = [math]::Sin(2 * [math]::PI * $Hz * ($i - $start) / $Rate) * 0.6
        $s = [int16][math]::Round($v * 32767)
        $data[$i * 2] = [byte]($s -band 0xFF)
        $data[$i * 2 + 1] = [byte](($s -shr 8) -band 0xFF)
    }
    $ms = New-Object System.IO.MemoryStream
    $bw = New-Object System.IO.BinaryWriter($ms)
    $bw.Write([char[]]'RIFF')
    $bw.Write([int](36 + $data.Length))
    $bw.Write([char[]]'WAVE')
    $bw.Write([char[]]'fmt ')
    $bw.Write([int]16)
    $bw.Write([int16]1)
    $bw.Write([int16]1)
    $bw.Write([int]$Rate)
    $bw.Write([int]($Rate * 2))
    $bw.Write([int16]2)
    $bw.Write([int16]16)
    $bw.Write([char[]]'data')
    $bw.Write([int]$data.Length)
    $bw.Write($data)
    $bw.Flush()
    [System.IO.File]::WriteAllBytes($Path, $ms.ToArray())
    $bw.Dispose(); $ms.Dispose()
}

$toneWav = Join-Path $Artifacts 'tone880.wav'
$replayWav = Join-Path $Artifacts 'replay_880.wav'
New-ToneWav -Path $toneWav -Hz $ToneHz -Seconds $ToneSeconds
New-ReplayWav -Path $replayWav -LeadS 0.5 -ToneS 0.5 -TailS 0.5 -Hz $ToneHz
Write-Host "  测试音 : $toneWav ($ToneHz Hz / $ToneSeconds s)"
Write-Host "  回放素材: $replayWav (0.5 s 静音 + 0.5 s $ToneHz Hz + 0.5 s 静音)"

# 用 CLI 自己建一个只含 880 Hz 的库 + 索引（同一条链路，不依赖 python 等工具）
$libDir = $Artifacts
$libPath = Join-Path $libDir 'cli_library.srz'
if (Test-Path $libPath) { Remove-Item -Force $libPath }
Write-Host "  用 live 的索引自动重建能力准备库/索引…"
# 库里没有条目时 live 会拒绝运行，所以先用 serve 的 API 录一条：
# 这里直接用 artifacts\p2\cli_library.srz（P2 主链路验证用过的库，含 880/1500 Hz）。
$libPath = Join-Path $ModuleDir 'artifacts\p2\cli_library.srz'
if (-not (Test-Path $libPath)) {
    throw "找不到 $libPath；请先跑 P2 的验证生成该库，或用 serve 建库后重跑本脚本"
}
Write-Host "  复用 P2 库: $libPath"

$idxPath = Join-Path $Artifacts 'cli_library.index.bin'
if (Test-Path $idxPath) { Remove-Item -Force $idxPath }

# ---------------------------------------------------------------------------
# A. 文件回放识别（不依赖声卡）
# ---------------------------------------------------------------------------
Section 'A. 文件回放识别（live --wav，不依赖声卡）'

$csvA = Join-Path $Artifacts 'replay_hits.csv'
if (Test-Path $csvA) { Remove-Item -Force $csvA }
$jsonA = Join-Path $Artifacts 'replay.jsonl'
if (Test-Path $jsonA) { Remove-Item -Force $jsonA }

$liveArgsA = @('live', '--wav', $replayWav, '--library', $libPath, '--index', $idxPath,
               '--top', '5', '--csv', $csvA, '--json')
Write-Host "  > soundradar $($liveArgsA -join ' ')"
$outA = & $Exe @liveArgsA 2>&1 | Out-String
$codeA = $LASTEXITCODE
[System.IO.File]::WriteAllText((Join-Path $Artifacts 'A_replay_stdout.txt'), $outA, [System.Text.UTF8Encoding]::new($false))

$linesA = @($outA -split "`r?`n" | Where-Object { $_.Trim() -ne '' })
$ticksA = @($linesA | Where-Object { $_ -match '"type":"tick"' })
$eventsA = @($linesA | Where-Object { $_ -match '"type":"event"' })
$summaryA = @($linesA | Where-Object { $_ -match '"type":"summary"' })
Write-Host "  JSONL: tick=$($ticksA.Count) event=$($eventsA.Count) summary=$($summaryA.Count)（exit=$codeA）"
if ($eventsA.Count -gt 0) {
    Write-Host "  首个 event 行:" -ForegroundColor DarkGray
    Write-Host "    $($eventsA[0])"
}
if ($ticksA.Count -ge 3) {
    Write-Host "  第 3 条 tick:" -ForegroundColor DarkGray
    Write-Host "    $($ticksA[2])"
}

$passA = $true
$detailA = ''
if ($codeA -ne 0) { $passA = $false; $detailA = "live 退出码 $codeA" }
elseif ($eventsA.Count -eq 0) { $passA = $false; $detailA = 'JSONL 里没有 event 记录' }
elseif ($ticksA.Count -lt 5) { $passA = $false; $detailA = "tick 只有 $($ticksA.Count) 条" }
elseif (-not ($eventsA[0] -match '880')) { $passA = $false; $detailA = "命中不是 880 Hz: $($eventsA[0])" }

# CSV 断言
$csvRowA = $null
if (Test-Path $csvA) {
    $rowsA = Import-Csv -Path $csvA -Encoding UTF8
    $csvRowA = $rowsA | Where-Object { $_.'名称' -match '880' } | Select-Object -First 1
}
if ($passA -and -not $csvRowA) { $passA = $false; $detailA = 'CSV 里没有 880 Hz 的命中行' }
if ($csvRowA) {
    Write-Host "  CSV 命中行: 时间=$($csvRowA.'时间') 条目=$($csvRowA.'条目ID') 名称=$($csvRowA.'名称') 分数=$($csvRowA.'分数') margin=$($csvRowA.'margin') 电平=$($csvRowA.'电平dBFS') dBFS"
}

if ($passA) {
    Add-Result 'A 文件回放命中 880 Hz' 'PASS' ("event=$($eventsA.Count) tick=$($ticksA.Count) 分数=" + ($csvRowA.'分数'))
} else {
    Add-Result 'A 文件回放命中 880 Hz' 'FAIL' $detailA
}

# 负对照：回放一个未注册的频率
$negWav = Join-Path $Artifacts 'replay_220.wav'
New-ReplayWav -Path $negWav -LeadS 0.2 -ToneS 1.0 -TailS 0.2 -Hz 220
$csvN = Join-Path $Artifacts 'neg_hits.csv'
if (Test-Path $csvN) { Remove-Item -Force $csvN }
$outN = & $Exe live --wav $negWav --library $libPath --index $idxPath --csv $csvN --quiet 2>&1 | Out-String
$negHits = 0
if (Test-Path $csvN) {
    $negRows = Import-Csv -Path $csvN -Encoding UTF8
    $negHits = @($negRows).Count
}
if ($negHits -eq 0) {
    Add-Result 'A 负对照（220 Hz 未注册）' 'PASS' '0 个命中'
} else {
    Add-Result 'A 负对照（220 Hz 未注册）' 'FAIL' "$negHits 个命中"
}

# ---------------------------------------------------------------------------
# B. 真实采集链路（播放 = 采集端点）
# ---------------------------------------------------------------------------
Section "B. 真实采集链路（$CaptureSeconds 秒 loopback + 播放 $ToneHz Hz）"

# 先确认端点枚举正常
$devOut = & $Exe devices 2>&1 | Out-String
$devLines = @($devOut -split "`r?`n" | Where-Object { $_ -match '^\s*\[\d+\]' })
Write-Host "  可用渲染端点: $($devLines.Count) 个"
$devLines | ForEach-Object { Write-Host "    $_" }

$csvB = Join-Path $Artifacts 'capture_hits.csv'
$outB = Join-Path $Artifacts 'capture_stdout.txt'
$errB = Join-Path $Artifacts 'capture_stderr.txt'
if (Test-Path $csvB) { Remove-Item -Force $csvB }
if (Test-Path $outB) { Remove-Item -Force $outB }
if (Test-Path $errB) { Remove-Item -Force $errB }

$captureArgs = @('live', '--seconds', "$CaptureSeconds", '--library', $libPath, '--index', $idxPath,
                 '--csv', $csvB, '--top', '5')
Write-Host "  > soundradar $($captureArgs -join ' ')"

$swB = [Diagnostics.Stopwatch]::StartNew()
# 注意：Start-Process -PassThru -NoNewWindow -RedirectStandardOutput 在
# PowerShell 5.1 下拿不到 ExitCode（实测为空），所以这里直接用 .NET Process。
$psiB = New-Object System.Diagnostics.ProcessStartInfo
$psiB.FileName = $Exe
$psiB.Arguments = ($captureArgs -join ' ')
$psiB.UseShellExecute = $false
$psiB.RedirectStandardOutput = $true
$psiB.RedirectStandardError = $true
$psiB.CreateNoWindow = $true
$procB = New-Object System.Diagnostics.Process
$procB.StartInfo = $psiB
$null = $procB.Start()
$outTaskB = $procB.StandardOutput.ReadToEndAsync()
$errTaskB = $procB.StandardError.ReadToEndAsync()
Start-Sleep -Milliseconds 1500

# 另起一个进程播放测试音（SoundPlayer 是同步播放，放在独立进程里循环）
$playScript = @"
`$player = New-Object Media.SoundPlayer '$toneWav'
`$deadline = (Get-Date).AddSeconds($CaptureSeconds + 2)
while ((Get-Date) -lt `$deadline) { `$player.PlaySync() }
"@
$playFile = Join-Path $Artifacts 'play_tone.ps1'
[System.IO.File]::WriteAllText($playFile, $playScript, [System.Text.UTF8Encoding]::new($true))
$procP = Start-Process -FilePath 'powershell.exe' -ArgumentList @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $playFile) -PassThru -WindowStyle Hidden

# 采集进程 CPU / 内存采样
$cpuSamples = @()
$memPeak = 0
while (-not $procB.HasExited) {
    try {
        $procB.Refresh()
        $cpuSamples += $procB.TotalProcessorTime.TotalMilliseconds
        if ($procB.WorkingSet64 -gt $memPeak) { $memPeak = $procB.WorkingSet64 }
    } catch { }
    Start-Sleep -Milliseconds 400
}
$swB.Stop()
$procB.WaitForExit()
$exitB = $procB.ExitCode
if (-not $procP.HasExited) { $procP.Kill() }
$procP.WaitForExit()

$outBText = $outTaskB.Result
$errBText = $errTaskB.Result
if (-not $outBText) { $outBText = '' }
if (-not $errBText) { $errBText = '' }
[System.IO.File]::WriteAllText($outB, $outBText, [System.Text.UTF8Encoding]::new($false))
[System.IO.File]::WriteAllText($errB, $errBText, [System.Text.UTF8Encoding]::new($false))
Write-Host "  live 退出码: $exitB，墙钟 $( [math]::Round($swB.Elapsed.TotalSeconds,2) ) s"

# 打印统计块
$statLines = @($outBText -split "`r?`n" | Where-Object { $_ -match '^\[live\]' })
Write-Host '  ---- live 输出（统计部分）----' -ForegroundColor DarkGray
$statLines | Where-Object { $_ -match '实时统计|音频块|打分窗口|tick / 命中|丢帧|处理耗时|堆内存|结束时|音源|采集格式|索引规模|特征参数|判定门槛' } |
    ForEach-Object { Write-Host "    $_" }

$capHits = @()
if (Test-Path $csvB) {
    $capRows = Import-Csv -Path $csvB -Encoding UTF8
    $capHits = @($capRows)
}
$toneHit = $capHits | Where-Object { $_.'名称' -match '880' } | Select-Object -First 1

if ($toneHit) {
    Write-Host "  采集命中行: 时间=$($toneHit.'时间') 条目=$($toneHit.'条目ID') 名称=$($toneHit.'名称') 分数=$($toneHit.'分数') margin=$($toneHit.'margin') 电平=$($toneHit.'电平dBFS') dBFS" -ForegroundColor Green
    Add-Result 'B 真实采集命中 880 Hz' 'PASS' ("CSV 命中 $($capHits.Count) 条，首条 分数=$($toneHit.'分数')")
} else {
    $why = '未命中'
    if ($errBText -match 'blocked|Application Control') { $why = 'exe 被 Smart App Control 拦截: ' + ($errBText -split "`r?`n")[0] }
    elseif ($outBText -match '没有可用|ErrNoEndpoint|no active audio') { $why = '本机没有可用渲染端点' }
    elseif ($capHits.Count -gt 0) { $why = "命中 $($capHits.Count) 条但没有 880 Hz: " + (($capHits | Select-Object -First 3 | ForEach-Object { $_.'名称' }) -join ', ') }
    elseif ($outBText -match '静音跳过 (\d+)' -and $outBText -notmatch '0 采样') { }
    Write-Host "  采集链路未命中：$why" -ForegroundColor Yellow
    Write-Host "  stderr: $($errBText.Trim())" -ForegroundColor DarkGray
    Add-Result 'B 真实采集命中 880 Hz' 'FAIL' $why
}

# ---------------------------------------------------------------------------
# C. 实时统计（耗时 / CPU / 内存）
# ---------------------------------------------------------------------------
Section 'C. 实时统计（单块处理耗时、CPU、内存）'

$avgMs = $null; $maxMs = $null; $per20 = $null; $heap = $null
$m = [regex]::Match($outBText, '处理耗时\s*:\s*单块平均\s*([\d.]+)\s*ms，峰值\s*([\d.]+)\s*ms；每 20 ms 音频\s*([\d.]+)\s*ms')
if ($m.Success) {
    $avgMs = [double]$m.Groups[1].Value
    $maxMs = [double]$m.Groups[2].Value
    $per20 = [double]$m.Groups[3].Value
}
$mh = [regex]::Match($outBText, '堆内存\s*:\s*([\d.]+)\s*MiB')
if ($mh.Success) { $heap = [double]$mh.Groups[1].Value }

if ($avgMs -ne $null) {
    Write-Host ("  单块平均处理耗时: {0:N3} ms，峰值 {1:N3} ms" -f $avgMs, $maxMs)
    Write-Host ("  每 20 ms 音频   : {0:N3} ms（预算 10 ms）" -f $per20)
    if ($per20 -lt 10) {
        Add-Result 'C 处理耗时 < 10 ms / 20 ms 音频' 'PASS' ("{0:N3} ms" -f $per20)
    } else {
        Add-Result 'C 处理耗时 < 10 ms / 20 ms 音频' 'FAIL' ("{0:N3} ms" -f $per20)
    }
} else {
    Add-Result 'C 处理耗时 < 10 ms / 20 ms 音频' 'SKIP' '没有解析到统计行（真实采集链路没跑成）'
}

if ($cpuSamples.Count -gt 1) {
    $cpuDelta = $cpuSamples[-1] - $cpuSamples[0]
    $wallMs = $swB.Elapsed.TotalMilliseconds
    $cores = [Environment]::ProcessorCount
    $cpuPct = if ($wallMs -gt 0) { 100.0 * $cpuDelta / ($wallMs * $cores) } else { 0 }
    Write-Host ("  CPU 占用       : {0:N2}%（{1:N0} ms CPU / {2:N0} ms 墙钟 × {3} 核）" -f $cpuPct, $cpuDelta, $wallMs, $cores)
    Write-Host ("  峰值工作集     : {0:N1} MiB" -f ($memPeak / 1MB))
    if ($heap -ne $null) { Write-Host ("  结束堆内存     : {0:N1} MiB (HeapInuse)" -f $heap) }
    Add-Result 'C CPU 占用' 'PASS' ("{0:N2}%" -f $cpuPct)
} else {
    Add-Result 'C CPU 占用' 'SKIP' '采集进程没有产生 CPU 采样'
}

# ---------------------------------------------------------------------------
# D. SSE 端点（真实 HTTP，不需要声卡：用 --wav 起会话）
# ---------------------------------------------------------------------------
Section 'D. SSE 实时打分端点（真实 HTTP）'

$port = 8899
$outS = Join-Path $Artifacts 'serve.stdout.txt'
$errS = Join-Path $Artifacts 'serve.stderr.txt'
$sseWav = Join-Path $Artifacts 'sse_replay_880.wav'
New-ReplayWav -Path $sseWav -LeadS 1.0 -ToneS 3.0 -TailS 0.5 -Hz $ToneHz
$srv = Start-Process -FilePath $Exe -ArgumentList @('serve', '--port', "$port", '--strict-port', '--library', $libPath, '--quiet') `
    -PassThru -NoNewWindow -RedirectStandardOutput $outS -RedirectStandardError $errS
Start-Sleep -Seconds 2
$passD = $false
$detailD = ''
try {
    # realtime=true 让 4.5 s 的素材按真实速度播，SSE 才有连续的 tick 可读
    $start = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$port/api/live/start" -ContentType 'application/json' `
        -Body (@{ wav = $sseWav; realtime = $true; topN = 5 } | ConvertTo-Json)
    Write-Host "  /api/live/start -> running=$($start.running) 源=$($start.source)"
    $state = Invoke-RestMethod -Uri "http://127.0.0.1:$port/api/live"
    Write-Host "  /api/live       -> running=$($state.running) 事件=$($state.eventCount) 订阅者=$($state.subscribers) tickMs=$($state.tickMs)"
    $devs = Invoke-RestMethod -Uri "http://127.0.0.1:$port/api/live/devices"
    Write-Host "  /api/live/devices -> $($devs.devices.Count) 个端点"

    # 读 SSE：用 HttpWebRequest 流式读取，最多 10 秒
    $req = [System.Net.HttpWebRequest]::Create("http://127.0.0.1:$port/api/live/stream")
    $req.Timeout = 10000
    $req.ReadWriteTimeout = 10000
    $resp = $req.GetResponse()
    Write-Host "  SSE Content-Type: $($resp.ContentType)"
    $stream = $resp.GetResponseStream()
    $reader = New-Object System.IO.StreamReader($stream)
    $tickCount = 0; $eventCount = 0; $sampleTick = ''; $sampleEvent = ''
    $stopAt = (Get-Date).AddSeconds(10)
    while ((Get-Date) -lt $stopAt) {
        $line = $reader.ReadLine()
        if ($line -eq $null) { break }
        if ($line -like 'event: tick') {
            $data = $reader.ReadLine()
            $tickCount++
            if ($sampleTick -eq '' -and $data -match '"top":\[\{') { $sampleTick = $data }
        } elseif ($line -like 'event: event') {
            $data = $reader.ReadLine()
            $eventCount++
            if ($sampleEvent -eq '') { $sampleEvent = $data }
        }
        if ($tickCount -ge 5 -and $eventCount -ge 1) { break }
    }
    $reader.Close(); $resp.Close()
    Write-Host "  SSE 收到: tick=$tickCount event=$eventCount"
    if ($sampleTick) { Write-Host "  tick 样例: $($sampleTick.Substring(0, [Math]::Min(300, $sampleTick.Length)))…" }
    if ($sampleEvent) { Write-Host "  event 样例: $sampleEvent" }
    $passD = ($tickCount -ge 5 -and $eventCount -ge 1 -and $sampleTick -match '"level"' -and $sampleTick -match '"top"')
    if (-not $passD) { $detailD = "tick=$tickCount event=$eventCount" }
    # 顺带确认 exe 内嵌的前端确实带上了实时打分页签（go:embed 是否吃到了新资源）
    $appJs = (Invoke-WebRequest -Uri "http://127.0.0.1:$port/app.js" -UseBasicParsing).Content
    $indexHtml = (Invoke-WebRequest -Uri "http://127.0.0.1:$port/" -UseBasicParsing).Content
    if ($appJs -match 'connectSSE' -and $appJs -match '/api/live/stream' -and $indexHtml -match '实时打分') {
        Add-Result 'D exe 内嵌前端含实时打分页签' 'PASS' ("app.js {0:N0} B / index.html {1:N0} B" -f $appJs.Length, $indexHtml.Length)
    } else {
        Add-Result 'D exe 内嵌前端含实时打分页签' 'FAIL' 'app.js 或 index.html 里找不到实时面板代码'
    }
    $null = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$port/api/live/stop"
} catch {
    $detailD = $_.Exception.Message
    Write-Host "  SSE 验证异常: $detailD" -ForegroundColor Yellow
} finally {
    if (-not $srv.HasExited) { $srv.Kill() }
}
if ($passD) {
    Add-Result 'D SSE /api/live/stream' 'PASS' "tick>=$tickCount，event>=$eventCount，字段含 level/top"
} else {
    Add-Result 'D SSE /api/live/stream' 'FAIL' $detailD
}

# ---------------------------------------------------------------------------
# E. 前端接线（node + DOM 桩，无需浏览器）
# ---------------------------------------------------------------------------
Section 'E. 实时打分面板前端接线（node DOM 桩）'

$smoke = Join-Path $ModuleDir 'tools\ui_smoke.js'
if (-not (Test-Path $smoke)) {
    Add-Result 'E UI 冒烟测试' 'SKIP' "找不到 $smoke"
} else {
    Push-Location $ModuleDir
    try {
        $nodeOut = & node $smoke 2>&1 | Out-String
        $nodeExit = $LASTEXITCODE
        [System.IO.File]::WriteAllText((Join-Path $Artifacts 'ui_smoke.txt'), $nodeOut, [System.Text.UTF8Encoding]::new($false))
        foreach ($l in ($nodeOut -split "`r?`n" | Where-Object { $_ -match '^\s*\[' })) { Write-Host "  $l" }
        if ($nodeExit -eq 0) {
            $n = ([regex]::Match($nodeOut, '全部通过（(\d+) 项）')).Groups[1].Value
            Add-Result 'E UI 冒烟测试' 'PASS' "$n 项断言全部通过"
        } else {
            $failsUi = @($nodeOut -split "`r?`n" | Where-Object { $_ -match '^\s+- ' })
            Add-Result 'E UI 冒烟测试' 'FAIL' ($failsUi -join ' | ')
        }
    } finally {
        Pop-Location
    }
}

# ---------------------------------------------------------------------------
# F. 已有子命令回归
# ---------------------------------------------------------------------------
Section 'F. 已有子命令回归（devices/capture/serve/version/index/match/live）'

Push-Location $ModuleDir
try {
    # 每个子命令都做一次真实调用，确认 P0/P1/P2 的行为没被破坏。
    # 注意：PowerShell 5.1 下 native exe 往 stderr 写东西 + 2>&1 会变成
    # NativeCommandError，在 $ErrorActionPreference='Stop' 下会直接终止脚本，故包 try/catch。
    function Invoke-Exe {
        param([Parameter(Mandatory = $true)][string[]]$ExeArgs)
        try { return (& $Exe @ExeArgs 2>&1 | Out-String) }
        catch { return ($_.Exception.Message + "`n") }
    }

    $sub = @()

    $vOut = Invoke-Exe @('version')
    $sub += [pscustomobject]@{ Cmd = 'version'; Exit = $LASTEXITCODE; Ok = ($vOut -match 'soundradar 0\.4'); Note = (($vOut -split "`r?`n")[0]).Trim() }

    $dOut = Invoke-Exe @('devices')
    $sub += [pscustomobject]@{ Cmd = 'devices'; Exit = $LASTEXITCODE; Ok = ($dOut -match 'Active render endpoints'); Note = (($dOut -split "`r?`n" | Where-Object { $_ -match '^\s*\[\d+\]' } | Select-Object -First 1)).Trim() }

    $capWav = Join-Path $Artifacts 'regress_capture.wav'
    $capOut = Invoke-Exe @('capture', '--seconds', '1', '--out', $capWav)
    $sub += [pscustomobject]@{ Cmd = 'capture'; Exit = $LASTEXITCODE; Ok = ($capOut -match 'frames' -and (Test-Path $capWav)); Note = (($capOut -split "`r?`n" | Where-Object { $_ -match 'frames' } | Select-Object -First 1)).Trim() }

    $idxBin = Join-Path $Artifacts 'regress_index.bin'
    $idxOut = Invoke-Exe @('index', 'rebuild', '--library', $libPath, '--out', $idxBin)
    $sub += [pscustomobject]@{ Cmd = 'index rebuild'; Exit = $LASTEXITCODE; Ok = ($idxOut -match '条目数' -and (Test-Path $idxBin)); Note = (($idxOut -split "`r?`n" | Where-Object { $_ -match '条目数|样本数' }) -join ' / ').Trim() }

    $mOut = Invoke-Exe @('match', '--wav', (Join-Path $ModuleDir 'artifacts\p2\tone880.wav'), '--library', $libPath, '--index', $idxBin)
    $sub += [pscustomobject]@{ Cmd = 'match'; Exit = $LASTEXITCODE; Ok = ($mOut -match '结论' -and $mOut -match '880'); Note = (($mOut -split "`r?`n" | Where-Object { $_ -match '^结论' } | Select-Object -First 1)).Trim() }

    $lhOut = Invoke-Exe @('live', '--help')
    $sub += [pscustomobject]@{ Cmd = 'live --help'; Exit = $LASTEXITCODE; Ok = ($lhOut -match '--csv' -and $lhOut -match '实时识别'); Note = '中文用法输出到 stdout' }

    # serve：真实启动 + 拉一次 /api/library，确认 P1 管理端没被影响
    $servePort = 8897
    $serveOut = Join-Path $Artifacts 'regress_serve.out.txt'
    $serveErr = Join-Path $Artifacts 'regress_serve.err.txt'
    $svp = Start-Process -FilePath $Exe -ArgumentList @('serve', '--port', "$servePort", '--strict-port', '--library', $libPath, '--quiet') `
        -PassThru -NoNewWindow -RedirectStandardOutput $serveOut -RedirectStandardError $serveErr
    Start-Sleep -Seconds 2
    $serveOk = $false
    $serveNote = ''
    try {
        $libDto = Invoke-RestMethod -Uri "http://127.0.0.1:$servePort/api/library"
        $hm = Invoke-WebRequest -Uri "http://127.0.0.1:$servePort/" -UseBasicParsing
        $serveOk = ($libDto.itemCount -ge 1 -and $hm.Content -match 'SoundRadar')
        $serveNote = "条目 $($libDto.itemCount)，首页 $($hm.Content.Length) B"
    } catch {
        $serveNote = $_.Exception.Message
    } finally {
        if (-not $svp.HasExited) { $svp.Kill() }
    }
    $sub += [pscustomobject]@{ Cmd = 'serve + /api/library'; Exit = 0; Ok = $serveOk; Note = $serveNote }

    foreach ($s in $sub) {
        if ($s.Ok) { Add-Result "子命令 $($s.Cmd)" 'PASS' $s.Note } else { Add-Result "子命令 $($s.Cmd)" 'FAIL' ("exit=$($s.Exit) " + $s.Note) }
    }
} finally {
    Pop-Location
}

# ---------------------------------------------------------------------------
# G. 离线回归
# ---------------------------------------------------------------------------
if (-not $SkipRegression) {
    Section 'G. 离线回归（build / vet / gofmt / test）'

    Push-Location $ModuleDir
    try {
        & go build ./... 2>&1 | Out-Host
        if ($LASTEXITCODE -eq 0) { Add-Result 'go build ./...' 'PASS' '' } else { Add-Result 'go build ./...' 'FAIL' "exit=$LASTEXITCODE" }

        $vetOut = & go vet ./... 2>&1 | Out-String
        if ($LASTEXITCODE -eq 0) { Add-Result 'go vet ./...' 'PASS' '' } else { Add-Result 'go vet ./...' 'FAIL' $vetOut.Trim() }

        $fmtOut = & gofmt -l . 2>&1 | Out-String
        if ($fmtOut.Trim() -eq '') { Add-Result 'gofmt -l .' 'PASS' '无未格式化文件' } else { Add-Result 'gofmt -l .' 'FAIL' $fmtOut.Trim() }

        # ---- go test ./... --------------------------------------------------
        # 主检查就是这一条。Smart App Control 会按文件哈希不稳定地拦截未签名的
        # 测试二进制（同一个包这次能跑下次被拦），所以失败的包再用
        # go test -c + Start-Process 重试，两层都拦就如实报 BLOCKED。
        $allOut = & go test ./... 2>&1 | Out-String
        $allExit = $LASTEXITCODE
        [System.IO.File]::WriteAllText((Join-Path $Artifacts 'gotest_all.txt'), $allOut, [System.Text.UTF8Encoding]::new($false))
        $pkgLines = @($allOut -split "`r?`n" | Where-Object { $_ -match '^(ok|FAIL|\?|---)' })
        $pkgLines | ForEach-Object { Write-Host "    $_" }
        $failedPkgs = @()
        foreach ($l in ($allOut -split "`r?`n")) {
            $mm = [regex]::Match($l, '^FAIL\s+(\S+)')
            if ($mm.Success) { $failedPkgs += $mm.Groups[1].Value }
        }
        if ($allExit -eq 0) {
            Add-Result 'go test ./...' 'PASS' ("全部包通过（{0} 行结果）" -f $pkgLines.Count)
        } else {
            Add-Result 'go test ./...' 'FAIL' ("失败的包: " + ($failedPkgs -join ', '))
        }

        # ---- 逐包重试（只针对 go test ./... 里失败的包）----------------------
        $verifyDir = Join-Path $Artifacts 'verify'
        if (-not (Test-Path $verifyDir)) { New-Item -ItemType Directory -Force -Path $verifyDir | Out-Null }
        $pkgResults = @()
        $pkgs = @(
            @{ Name = 'dsp'; Path = './internal/dsp' },
            @{ Name = 'match'; Path = './internal/match' },
            @{ Name = 'live'; Path = './internal/live' },
            @{ Name = 'server'; Path = './internal/server' },
            @{ Name = 'audio'; Path = './internal/audio' },
            @{ Name = 'library'; Path = './internal/library' },
            @{ Name = 'capture'; Path = './internal/capture' },
            @{ Name = 'index'; Path = './internal/index' },
            @{ Name = 'cmd'; Path = './cmd/soundradar' }
        )
        foreach ($pkg in $pkgs) {
            $name = $pkg.Name
            $isFailed = $false
            foreach ($f in $failedPkgs) { if ($f -like "*/$name" -or $f -like "*\$name") { $isFailed = $true } }
            if (-not $isFailed) {
                $pkgResults += [pscustomobject]@{ Pkg = $name; Verdict = 'OK'; Out = 'go test ./... 里通过' }
                continue
            }
            $testExe = Join-Path $verifyDir "$name.test.exe"
            if (Test-Path $testExe) { Remove-Item -Force $testExe }
            $built = $false
            for ($try = 1; $try -le 3; $try++) {
                & go test -c -o $testExe $pkg.Path 2>&1 | Out-Host
                if ($LASTEXITCODE -eq 0) { $built = $true; break }
                Start-Sleep -Milliseconds 300
            }
            if (-not $built) { $pkgResults += [pscustomobject]@{ Pkg = $name; Verdict = 'BUILDFAIL'; Out = '' }; continue }
            if (-not (Test-Path $testExe)) {
                # 该包没有测试文件（go test -c 不产出二进制）
                $pkgResults += [pscustomobject]@{ Pkg = $name; Verdict = 'NOTEST'; Out = '' }
                continue
            }
            $pkgOutFile = Join-Path $verifyDir "$name.out.txt"
            $pkgErrFile = Join-Path $verifyDir "$name.err.txt"
            $ran = $false
            $blocked = $false
            for ($try = 1; $try -le 3; $try++) {
                try {
                    # Smart App Control 被拦时 Start-Process 抛 Win32Exception；在
                    # $ErrorActionPreference=Stop 下必须 catch，否则整个脚本终止。
                    $p = Start-Process -FilePath $testExe -ArgumentList @('-test.v') -Wait -PassThru `
                        -RedirectStandardOutput $pkgOutFile -RedirectStandardError $pkgErrFile
                    if ($p.ExitCode -eq 0) { $ran = $true; break }
                    $errText = if (Test-Path $pkgErrFile) { Get-Content -Raw $pkgErrFile } else { '' }
                    if ($errText -match 'Access is denied|Application Control') {
                        $blocked = $true
                        Start-Sleep -Milliseconds 500
                        continue
                    }
                    break
                } catch {
                    $blocked = $true
                    Write-Host "    ($name.test.exe 第 $try 次启动被拦: $($_.Exception.Message))" -ForegroundColor Yellow
                    Start-Sleep -Milliseconds 500
                }
            }
            $pkgOut = if (Test-Path $pkgOutFile) { Get-Content -Raw $pkgOutFile } else { '' }
            $pkgErrText2 = if (Test-Path $pkgErrFile) { Get-Content -Raw $pkgErrFile } else { '' }
            if ($ran) {
                $verdict = 'PASS'
            } elseif ($blocked -or $pkgErrText2 -match 'Access is denied|Application Control') {
                $verdict = 'BLOCKED'
            } else {
                $verdict = 'FAIL'
            }
            $pkgResults += [pscustomobject]@{
                Pkg     = $name
                Verdict = $verdict
                Out     = (($pkgOut -split "`r?`n") | Where-Object { $_ -match '^(ok|FAIL|---|PASS)' } | Select-Object -First 3) -join ' / '
            }
        }
        foreach ($r in $pkgResults) {
            switch ($r.Verdict) {
                'OK' { }
                'PASS' { Add-Result "go test -c + 运行 ($($r.Pkg))" 'PASS' "重试成功 $($r.Out)" }
                'NOTEST' { }
                'BLOCKED' { Add-Result "go test -c + 运行 ($($r.Pkg))" 'FAIL' 'BLOCKED by Smart App Control（重试 3 次仍被拦）' }
                default { Add-Result "go test -c + 运行 ($($r.Pkg))" 'FAIL' "$($r.Verdict) $($r.Out)" }
            }
        }
    } finally {
        Pop-Location
    }
} else {
    Write-Host ''
    Write-Host '  （已按要求跳过离线回归）' -ForegroundColor Yellow
}

# ---------------------------------------------------------------------------
# 总结
# ---------------------------------------------------------------------------
Section '总结'
$script:Results | Format-Table -AutoSize Name, Verdict, Detail | Out-String -Width 200 | Write-Host
$fails = @($script:Results | Where-Object { $_.Verdict -eq 'FAIL' })
$skips = @($script:Results | Where-Object { $_.Verdict -eq 'SKIP' })
Write-Host ("  PASS {0} / FAIL {1} / SKIP {2}" -f @($script:Results | Where-Object { $_.Verdict -eq 'PASS' }).Count, $fails.Count, $skips.Count)
Write-Host "  证据文件目录: $Artifacts"

if ($fails.Count -gt 0) { exit 1 }
exit 0
