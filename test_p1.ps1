# P1 real-HTTP verification helpers for the soundradar library API.
#
# Windows PowerShell 5.1 (this sandbox's shell) has no `Invoke-RestMethod -Form`,
# and its Invoke-WebRequest switches to Transfer-Encoding: chunked when given a
# byte[] body, which the multipart parser rejects. All uploads therefore go
# through System.Net.HttpWebRequest with an explicit ContentLength.

$Script:Boundary = "----SoundRadarP1Boundary7f3a1c"

function New-MultipartBody {
    param(
        [hashtable]$Fields = @{},
        [hashtable]$Files = @{}
    )
    $ms = New-Object System.IO.MemoryStream

    foreach ($k in $Fields.Keys) {
        $head = "--$Script:Boundary`r`n" +
                "Content-Disposition: form-data; name=`"$k`"`r`n`r`n"
        $hb = [System.Text.Encoding]::UTF8.GetBytes($head)
        $ms.Write($hb, 0, $hb.Length)
        $vb = [System.Text.Encoding]::UTF8.GetBytes("$($Fields[$k])`r`n")
        $ms.Write($vb, 0, $vb.Length)
    }

    foreach ($k in $Files.Keys) {
        $spec = $Files[$k]
        $path = $spec['Path']
        if ($spec.ContainsKey('FileName') -and $spec['FileName']) { $fname = $spec['FileName'] } else { $fname = [System.IO.Path]::GetFileName($path) }
        $bytes = [System.IO.File]::ReadAllBytes($path)
        $ctype = 'application/octet-stream'
        if ($fname -match '\.mp3$') { $ctype = 'audio/mpeg' }
        elseif ($fname -match '\.(wav|wave)$') { $ctype = 'audio/wav' }
        elseif ($fname -match '\.png$') { $ctype = 'image/png' }
        elseif ($fname -match '\.jpe?g$') { $ctype = 'image/jpeg' }

        $head = "--$Script:Boundary`r`n" +
                "Content-Disposition: form-data; name=`"$k`"; filename=`"$fname`"`r`n" +
                "Content-Type: $ctype`r`n`r`n"
        $hb = [System.Text.Encoding]::UTF8.GetBytes($head)
        $ms.Write($hb, 0, $hb.Length)
        $ms.Write($bytes, 0, $bytes.Length)
        $tail = [System.Text.Encoding]::UTF8.GetBytes("`r`n")
        $ms.Write($tail, 0, $tail.Length)
    }

    $close = [System.Text.Encoding]::UTF8.GetBytes("--$Script:Boundary--`r`n")
    $ms.Write($close, 0, $close.Length)

    $out = $ms.ToArray()
    $ms.Dispose()
    return ,$out
}

function Get-MultipartContentType { return "multipart/form-data; boundary=$Script:Boundary" }

# Send-Request performs a raw HTTP call and returns
# @{ Status = <int>; StatusText = <string>; Headers = @{}; Body = <byte[]>; Text = <string> }
function Send-Request {
    param(
        [string]$Method,
        [string]$Url,
        [byte[]]$Body,
        [string]$ContentType,
        [hashtable]$Headers = @{}
    )
    $req = [System.Net.HttpWebRequest]::Create($Url)
    $req.Method = $Method
    $req.Timeout = 60000
    $req.AllowAutoRedirect = $false
    foreach ($k in $Headers.Keys) {
        if ($k -ieq 'Range') { $req.AddRange(0, [int]$Headers[$k]) } else { $req.Headers.Add($k, $Headers[$k]) }
    }
    if ($Body -ne $null) {
        $req.ContentType = $ContentType
        $req.ContentLength = $Body.Length
        $stream = $req.GetRequestStream()
        $stream.Write($Body, 0, $Body.Length)
        $stream.Close()
    }
    try {
        $resp = $req.GetResponse()
    } catch [System.Net.WebException] {
        $resp = $_.Exception.Response
        if ($null -eq $resp) { throw }
    }
    $ms = New-Object System.IO.MemoryStream
    $rs = $resp.GetResponseStream()
    $rs.CopyTo($ms)
    $rs.Close()
    $bytes = $ms.ToArray()
    $ms.Dispose()
    $text = [System.Text.Encoding]::UTF8.GetString($bytes)
    $h = @{}
    foreach ($key in $resp.Headers.AllKeys) { $h[$key] = $resp.Headers[$key] }
    $result = [pscustomobject]@{
        Status     = [int]$resp.StatusCode
        StatusText = $resp.StatusDescription
        Headers    = $h
        Body       = $bytes
        Text       = $text
    }
    $resp.Close()
    return $result
}

function Invoke-Json {
    param([string]$Method, [string]$Url, [string]$Json)
    $bytes = $null
    $ctype = $null
    if ($Json) {
        $bytes = [System.Text.Encoding]::UTF8.GetBytes($Json)
        $ctype = 'application/json; charset=utf-8'
    }
    return Send-Request -Method $Method -Url $Url -Body $bytes -ContentType $ctype
}

function Get-Json { param([string]$Url) return Invoke-Json -Method GET -Url $Url }

function Write-Section([string]$t) { Write-Output ""; Write-Output "=== $t ===" }
