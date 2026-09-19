# P1 end-to-end verification against a running `soundradar.exe serve --port 8765`.
# Usage:  .\verify_p1.ps1            (expects the server to be listening already)
$ErrorActionPreference = 'Stop'
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force

$root = Split-Path -Parent $MyInvocation.MyCommand.Path
. (Join-Path $root 'test_p1.ps1')

$base = 'http://127.0.0.1:8765'
$art  = Join-Path $root 'soundradar\artifacts\p1'

# ---------------------------------------------------------------- 1) create
Write-Section "1) POST /api/items  (audio=44100Hz/16-bit/mono wav, icon=300x180 PNG)"
$body = New-MultipartBody -Fields @{
    name = '测试音效'; tags = '测试,人工'; note = 'PowerShell 端到端上传'
    threshold = '0.66'; cooldownMs = '300'; profile = 'default'
} -Files @{
    audio = @{ Path = (Join-Path $art 't44k.wav') }
    icon  = @{ Path = (Join-Path $art 'i.png') }
}
$r1 = Send-Request -Method POST -Url "$base/api/items" -Body $body -ContentType (Get-MultipartContentType)
Write-Output "HTTP $($r1.Status) $($r1.StatusText)   Content-Type: $($r1.Headers['Content-Type'])"
Write-Output $r1.Text
$item = $r1.Text | ConvertFrom-Json
$id = $item.id

# ---------------------------------------------------------------- 2) library
Write-Section "2) GET /api/library"
$r2 = Get-Json "$base/api/library"
Write-Output "HTTP $($r2.Status)"
$lib = $r2.Text | ConvertFrom-Json
Write-Output ("path        = {0}" -f $lib.path)
Write-Output ("name        = {0}   schema={1}   createdAt={2}" -f $lib.name, $lib.schema, $lib.createdAt)
Write-Output ("itemCount   = {0}   sampleCount={1}   fileBytes={2}" -f $lib.itemCount, $lib.sampleCount, $lib.fileBytes)
Write-Output ("canonical   = {0} Hz / {1} ch / {2}-bit" -f $lib.sampleRate, $lib.channels, $lib.bitsPerSample)
Write-Output ("feature     = kind={0} rate={1} frame={2} hop={3} window={4} mel={5} fMin={6} fMax={7}" -f `
    $lib.feature.kind, $lib.feature.sampleRate, $lib.feature.frameSize, $lib.feature.hopSize, `
    $lib.feature.window, $lib.feature.melBands, $lib.feature.fMinHz, $lib.feature.fMaxHz)
Write-Output ("warnings    = {0}" -f ($lib.warnings.Count))
Write-Output ("items[0]    = id={0} name={1} samples={2} tags=[{3}] threshold={4} cooldownMs={5}" -f `
    $lib.items[0].id, $lib.items[0].name, $lib.items[0].sampleCount, ($lib.items[0].tags -join ','), `
    $lib.items[0].threshold, $lib.items[0].cooldownMs)

# ---------------------------------------------------------------- 3) detail
Write-Section "3) GET /api/items/$id"
$r3 = Get-Json "$base/api/items/$id"
Write-Output "HTTP $($r3.Status)"
$det = $r3.Text | ConvertFrom-Json
$s = $det.samples[0]
Write-Output ("sample[1]   = {0}  url={1}" -f $s.file, $s.url)
Write-Output ("  stored    = {0} Hz / {1} ch / {2}-bit, frames={3}, bytes={4}, peak={5:N2} dBFS" -f `
    $s.wavSampleRate, $s.wavChannels, $s.wavBitsPerSample, $s.frames, $s.bytes, $s.peakDbfs)
Write-Output ("  origin    = {0} | {1} | {2} Hz / {3} ch / {4}-bit | {5} s | resampled={6} downmixed={7}" -f `
    $s.origin.fileName, $s.origin.formatTag, $s.origin.sampleRate, $s.origin.channels, `
    $s.origin.bitsPerSample, $s.origin.durationS, $s.origin.resampled, $s.origin.downmixed)

# ---------------------------------------------------------------- 4) sample wav
Write-Section "4) GET /api/items/$id/samples/1.wav"
$r4 = Send-Request -Method GET -Url "$base/api/items/$id/samples/1.wav"
$magic = [System.Text.Encoding]::ASCII.GetString($r4.Body[0..3])
Write-Output ("HTTP {0}  bytes={1}  first4='{2}'  bytes8-11='{3}'  Content-Type={4}  Accept-Ranges={5}" -f `
    $r4.Status, $r4.Body.Length, $magic, [System.Text.Encoding]::ASCII.GetString($r4.Body[8..11]), `
    $r4.Headers['Content-Type'], $r4.Headers['Accept-Ranges'])
$hdr = @{}
$hdr['RIFF'] = [System.BitConverter]::ToUInt32($r4.Body, 4)
$hdr['fmt '] = [System.BitConverter]::ToUInt16($r4.Body, 20)
$hdr['channels'] = [System.BitConverter]::ToUInt16($r4.Body, 22)
$hdr['sampleRate'] = [System.BitConverter]::ToUInt32($r4.Body, 24)
$hdr['bits'] = [System.BitConverter]::ToUInt16($r4.Body, 34)
$hdr['dataBytes'] = [System.BitConverter]::ToUInt32($r4.Body, 40)
Write-Output ("  parsed WAV header: fmtTag={0} channels={1} rate={2} bits={3} dataBytes={4}" -f `
    $hdr['fmt '], $hdr['channels'], $hdr['sampleRate'], $hdr['bits'], $hdr['dataBytes'])

Write-Section "4b) GET same wav with Range: bytes=0-99"
$r4b = Send-Request -Method GET -Url "$base/api/items/$id/samples/1.wav" -Headers @{ Range = 99 }
Write-Output ("HTTP {0}  bytes={1}  Content-Range={2}" -f $r4b.Status, $r4b.Body.Length, $r4b.Headers['Content-Range'])

# ---------------------------------------------------------------- 5) icon
Write-Section "5) GET /api/items/$id/icon.png"
$r5 = Send-Request -Method GET -Url "$base/api/items/$id/icon.png"
$pngMagic = ($r5.Body[0..7] | ForEach-Object { $_.ToString('x2') }) -join ' '
Write-Output ("HTTP {0}  bytes={1}  magic={2}  Content-Type={3}" -f $r5.Status, $r5.Body.Length, $pngMagic, $r5.Headers['Content-Type'])
# PNG IHDR: width/height are big-endian at offset 16 and 20.
$w = [int]$r5.Body[16] * 16777216 + [int]$r5.Body[17] * 65536 + [int]$r5.Body[18] * 256 + [int]$r5.Body[19]
$h = [int]$r5.Body[20] * 16777216 + [int]$r5.Body[21] * 65536 + [int]$r5.Body[22] * 256 + [int]$r5.Body[23]
Write-Output ("  IHDR = {0} x {1}" -f $w, $h)

# ---------------------------------------------------------------- 6) PATCH json
Write-Section "6) PATCH /api/items/$id  (application/json)"
$r6 = Invoke-Json -Method PATCH -Url "$base/api/items/$id" -Json '{"name":"改名后的音效","tags":["改名","JSON"],"note":"通过 JSON PATCH 修改","threshold":0.55,"cooldownMs":900,"profile":"aggressive"}'
Write-Output "HTTP $($r6.Status)"
$patched = $r6.Text | ConvertFrom-Json
Write-Output ("name={0} threshold={1} cooldownMs={2} profile={3} tags=[{4}] note={5} samples={6}" -f `
    $patched.name, $patched.threshold, $patched.cooldownMs, $patched.profile, ($patched.tags -join ','), $patched.note, $patched.sampleCount)

# ---------------------------------------------------------------- 7) PATCH icon
Write-Section "7) PATCH /api/items/$id  (multipart, replaces the icon with the JPEG)"
$body7 = New-MultipartBody -Fields @{ patch = '{"name":"改名后的音效"}' } -Files @{ icon = @{ Path = (Join-Path $art 'i.jpg') } }
$r7 = Send-Request -Method PATCH -Url "$base/api/items/$id" -Body $body7 -ContentType (Get-MultipartContentType)
Write-Output "HTTP $($r7.Status)"
$r7b = Send-Request -Method GET -Url "$base/api/items/$id/icon.png"
Write-Output ("icon after replace: HTTP {0} bytes={1}  (before: {2} bytes)" -f $r7b.Status, $r7b.Body.Length, $r5.Body.Length)

# ---------------------------------------------------------------- 8) add sample
Write-Section "8) POST /api/items/$id/samples  (append an MP3 variant -> tests the MP3 decoder)"
$body8 = New-MultipartBody -Files @{ audio = @{ Path = (Join-Path $art 'silence.mp3') } }
$r8 = Send-Request -Method POST -Url "$base/api/items/$id/samples" -Body $body8 -ContentType (Get-MultipartContentType)
Write-Output "HTTP $($r8.Status)"
$two = $r8.Text | ConvertFrom-Json
Write-Output $r8.Text
foreach ($x in @($two.samples)) {
    Write-Output ("  sample[{0}] {1}  {2} Hz->{3} Hz  ch {4}->{5}  {6} frames  origin={7} resampled={8}" -f `
        $x.index, $x.file, $x.origin.sampleRate, $x.wavSampleRate, $x.origin.channels, $x.wavChannels, `
        $x.frames, $x.origin.container, $x.origin.resampled)
}

Write-Section "8b) GET /api/items/$id/samples/2.wav  (the MP3-derived sample)"
$r8b = Send-Request -Method GET -Url "$base/api/items/$id/samples/2.wav"
Write-Output ("HTTP {0}  bytes={1}  first4='{2}'" -f $r8b.Status, $r8b.Body.Length, [System.Text.Encoding]::ASCII.GetString($r8b.Body[0..3]))

# ---------------------------------------------------------------- 9) delete sample
Write-Section "9) DELETE /api/items/$id/samples/1"
$r9 = Send-Request -Method DELETE -Url "$base/api/items/$id/samples/1"
Write-Output "HTTP $($r9.Status)"
$afterDel = $r9.Text | ConvertFrom-Json
Write-Output ("remaining samples = {0}" -f @($afterDel.samples).Count)
Write-Output $r9.Text

# ---------------------------------------------------------------- 10) HTML
Write-Section "10) GET /  (embedded management UI)"
$r10 = Send-Request -Method GET -Url "$base/"
$hasTitle = $r10.Text.Contains('SoundRadar 音效库管理')
$hasForm = $r10.Text.Contains('新增音效条目')
Write-Output ("HTTP {0}  bytes={1}  Content-Type={2}  title-present={3}  form-present={4}" -f `
    $r10.Status, $r10.Body.Length, $r10.Headers['Content-Type'], $hasTitle, $hasForm)
$r10b = Send-Request -Method GET -Url "$base/app.js"
$r10c = Send-Request -Method GET -Url "$base/style.css"
Write-Output ("app.js: HTTP {0} {1} bytes   style.css: HTTP {2} {3} bytes" -f $r10b.Status, $r10b.Body.Length, $r10c.Status, $r10c.Body.Length)

# ---------------------------------------------------------------- 11) errors
Write-Section "11) error handling"
$bad = New-MultipartBody -Fields @{ name = '不支持格式测试' } -Files @{ audio = @{ Path = (Join-Path $art 'notaudio.ogg') } }
$r11 = Send-Request -Method POST -Url "$base/api/items" -Body $bad -ContentType (Get-MultipartContentType)
Write-Output ("POST .ogg  -> HTTP {0}  Content-Type={1}" -f $r11.Status, $r11.Headers['Content-Type'])
Write-Output ("  body: {0}" -f $r11.Text.Trim())
$r11b = Get-Json "$base/api/items/deadbeef"
Write-Output ("GET missing item -> HTTP {0}  body: {1}" -f $r11b.Status, $r11b.Text.Trim())
$r11c = Send-Request -Method PUT -Url "$base/api/library"
Write-Output ("PUT /api/library -> HTTP {0}  body: {1}" -f $r11c.Status, $r11c.Text.Trim())

# ---------------------------------------------------------------- 12) persistence
Write-Section "12) final state"
$r12 = Get-Json "$base/api/library"
$final = $r12.Text | ConvertFrom-Json
Write-Output ("items={0} samples={1} fileBytes={2}" -f $final.itemCount, $final.sampleCount, $final.fileBytes)
Write-Output ("ids: {0}" -f (($final.items | ForEach-Object { $_.id }) -join ','))
$id | Set-Content (Join-Path $root '.tmp_id.txt')
