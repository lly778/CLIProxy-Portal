#requires -Version 5.1
<#
Windows PowerShell + curl.exe only. No administrator rights or server changes.
Use the same parameters on the slow network and a hotspot, changing -Label.
Uploads deliberately use a nonexistent model: expected HTTP 400, no inference.
#>
[CmdletBinding()]
param(
    [string]$BaseUrl = 'http://cpa.lly778.xyz:8317/v1',
    [string]$Label = 'current-network',
    [ValidateRange(1, 10)][int]$Repeat = 2,
    [ValidateRange(5, 300)][int]$TimeoutSeconds = 60,
    [ValidateRange(256, 4194304)][int[]]$UploadBytes = @(1024, 65536, 241877),
    [string]$CompareIp = '',
    [string]$OutputDirectory = '',
    [switch]$SkipUpload,
    [switch]$TraceRoute,
    [switch]$NoPrompt
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'
$utf8 = New-Object System.Text.UTF8Encoding($false)
$invariant = [System.Globalization.CultureInfo]::InvariantCulture
$runId = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$diagnosticModel = '__portal_network_diagnostic_' + $runId
$temporaryDirectory = Join-Path ([IO.Path]::GetTempPath()) ('portal-net-' + $runId)
$apiKey = ''
$results = New-Object 'System.Collections.Generic.List[object]'

function Get-Field($Object, [string]$Name, $Default = $null) {
    if ($null -ne $Object -and $null -ne $Object.PSObject.Properties[$Name]) { return $Object.$Name }
    return $Default
}

function Hide-Key([string]$Value) {
    if ($apiKey) { return $Value.Replace($apiKey, '[REDACTED]') }
    return $Value
}

function Config-Value([string]$Value) {
    if ($Value -match '[\r\n\x00]') { throw 'Invalid newline/NUL in curl configuration value.' }
    return '"' + $Value.Replace('\', '\\').Replace('"', '\"') + '"'
}

function Seconds-ToMs($Value) {
    if ($null -eq $Value) { return $null }
    return [math]::Round([double]$Value * 1000, 3)
}

function Delta-Ms($End, $Start) {
    if ($null -eq $End -or $null -eq $Start) { return $null }
    return [math]::Round([math]::Max([double]0, ([double]$End - [double]$Start)) * 1000, 3)
}

function New-Payload([int]$Length) {
    $prefix = '{"model":"' + $diagnosticModel + '","messages":[{"role":"user","content":"'
    $suffix = '"}],"stream":false}'
    $paddingLength = $Length - $prefix.Length - $suffix.Length
    if ($paddingLength -lt 0) { throw 'UploadBytes is too small for the diagnostic JSON.' }
    $randomBytes = New-Object byte[] ([int][math]::Ceiling($paddingLength * 3 / 4) + 3)
    $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $rng.GetBytes($randomBytes) } finally { $rng.Dispose() }
    $padding = [Convert]::ToBase64String($randomBytes).Substring(0, $paddingLength)
    $path = Join-Path $temporaryDirectory ('payload-' + $Length + '.json')
    [IO.File]::WriteAllText($path, $prefix + $padding + $suffix, $utf8)
    return $path
}

function Curl-Block([string]$Kind, [string]$Route, [string]$BodyPath, [int]$Index) {
    $responsePath = Join-Path $temporaryDirectory ('response-' + $Index + '.txt')
    $headersPath = Join-Path $temporaryDirectory ('headers-' + $Index + '.txt')
    $url = $apiBase + '/models'
    if ($BodyPath) { $url = $apiBase + '/chat/completions' }
    $lines = @(
        'silent', 'show-error', 'http1.1',
        'connect-timeout = 10',
        ('max-time = ' + $TimeoutSeconds),
        'noproxy = "*"', 'proxy = ""',
        ('url = ' + (Config-Value $url)),
        ('output = ' + (Config-Value $responsePath)),
        ('dump-header = ' + (Config-Value $headersPath)),
        'write-out = "%{json}\n"',
        ('header = ' + (Config-Value ('X-Portal-Network-Diagnostic: ' + $runId))),
        'header = "Accept: application/json"'
    )
    if ($apiKey) { $lines += 'header = ' + (Config-Value ('Authorization: Bearer ' + $apiKey)) }
    if ($Route -eq 'fixed-ip') {
        $address = $CompareIp
        if ($address.Contains(':')) { $address = '[' + $address + ']' }
        $lines += 'resolve = ' + (Config-Value ($targetUri.DnsSafeHost + ':' + $targetUri.Port + ':' + $address))
    }
    if ($BodyPath) {
        $lines += @('header = "Content-Type: application/json"', 'header = "Expect:"',
            ('data-binary = ' + (Config-Value ('@' + $BodyPath))))
    }
    $requestedBytes = 0
    if ($BodyPath) { $requestedBytes = (Get-Item -LiteralPath $BodyPath).Length }
    return [pscustomobject]@{ Config = ($lines -join "`n"); Body = $responsePath; Headers = $headersPath; Kind = $Kind; Route = $Route; RequestedBytes = $requestedBytes }
}

function Invoke-Curl([object[]]$Blocks) {
    $configText = ($Blocks | ForEach-Object { $_.Config }) -join "`nnext`n"
    $start = Get-Date
    $info = New-Object Diagnostics.ProcessStartInfo
    $info.FileName = $curlPath
    # Key travels through stdin, never command-line arguments or a saved config.
    $info.Arguments = '--disable --config -'
    $info.UseShellExecute = $false
    $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true
    $info.RedirectStandardOutput = $true
    $info.RedirectStandardError = $true
    $process = New-Object Diagnostics.Process
    $process.StartInfo = $info
    $started = $false
    try {
        [void]$process.Start()
        $started = $true
        $stdoutTask = $process.StandardOutput.ReadToEndAsync()
        $stderrTask = $process.StandardError.ReadToEndAsync()
        $process.StandardInput.Write($configText + "`n")
        $process.StandardInput.Close()
        $watch = [Diagnostics.Stopwatch]::StartNew()
        $nextMessage = 10
        while (-not $process.WaitForExit(250)) {
            if ($watch.Elapsed.TotalSeconds -ge $nextMessage) {
                Write-Host ('  Still waiting: {0:N0}s (per-request limit {1}s)' -f $watch.Elapsed.TotalSeconds, $TimeoutSeconds)
                $nextMessage += 10
            }
            if ($watch.Elapsed.TotalSeconds -gt ($TimeoutSeconds + 15) * $Blocks.Count) {
                $process.Kill()
                throw 'curl exceeded the outer process time limit.'
            }
        }
        $stdout = $stdoutTask.Result
        $stderr = Hide-Key $stderrTask.Result
        $exitCode = $process.ExitCode
    } finally {
        if ($started -and -not $process.HasExited) { $process.Kill() }
        $process.Dispose()
    }
    $metrics = @($stdout -split '\r?\n' | Where-Object { $_.StartsWith('{') } | ForEach-Object { $_ | ConvertFrom-Json })
    if ($metrics.Count -ne $Blocks.Count) { throw ('curl returned unexpected metrics. ' + $stderr) }
    for ($i = 0; $i -lt $Blocks.Count; $i++) {
        $metric = $metrics[$i]
        $block = $Blocks[$i]
        $status = [int](Get-Field $metric 'http_code' 0)
        $body = ''
        if (Test-Path -LiteralPath $block.Body) { $body = [IO.File]::ReadAllText($block.Body) }
        $trace = ''
        if (Test-Path -LiteralPath $block.Headers) {
            $headerText = [IO.File]::ReadAllText($block.Headers)
            $traceMatch = [regex]::Match($headerText, '(?im)^x-cpa-trace-id:\s*([^\r\n]+)')
            if ($traceMatch.Success) { $trace = $traceMatch.Groups[1].Value.Trim() }
        }
        $code = [int](Get-Field $metric 'exitcode' $exitCode)
        $uploadSize = [long](Get-Field $metric 'size_upload' 0)
        $expected = ($status -eq 200 -and $block.Kind -ne 'upload') -or
            ($status -eq 401 -and -not $apiKey -and $block.Kind -ne 'upload') -or
            ($status -eq 400 -and $block.Kind -eq 'upload' -and $body.Contains($diagnosticModel) -and $uploadSize -eq $block.RequestedBytes)
        $expected = $expected -and ($code -eq 0)
        $postTransfer = $null
        if ($supportsPostTransfer) {
            $candidateTime = Get-Field $metric 'time_posttransfer'
            if ($null -ne $candidateTime -and [double]$candidateTime -gt 0) { $postTransfer = $candidateTime }
        }
        $bodySnippet = ''
        # Save only diagnostic error bodies, not successful model lists or headers.
        if ($status -ge 400 -and $body) {
            $bodySnippet = Hide-Key $body.Substring(0, [math]::Min(500, $body.Length))
        }
        $record = [pscustomobject][ordered]@{
            started_at_local = $start.ToString('o')
            kind = $block.Kind; route = $block.Route; curl_exit = $code
            http_status = $status; expected_response = [bool]$expected
            diagnostic_body_confirmed = [bool]($block.Kind -eq 'upload' -and $expected)
            requested_bytes = $block.RequestedBytes; uploaded_bytes = $uploadSize
            remote_ip = Get-Field $metric 'remote_ip' ''; remote_port = Get-Field $metric 'remote_port' 0
            local_ip = Get-Field $metric 'local_ip' ''; local_port = Get-Field $metric 'local_port' 0
            http_version = Get-Field $metric 'http_version' ''; new_connections = Get-Field $metric 'num_connects' 0
            dns_ms = Seconds-ToMs (Get-Field $metric 'time_namelookup')
            tcp_ms = Delta-Ms (Get-Field $metric 'time_connect') (Get-Field $metric 'time_namelookup')
            tls_ms = $(if ($targetUri.Scheme -eq 'https') { Delta-Ms (Get-Field $metric 'time_appconnect') (Get-Field $metric 'time_connect') } else { 0 })
            request_ready_ms = Seconds-ToMs (Get-Field $metric 'time_pretransfer')
            local_send_complete_ms = Seconds-ToMs $postTransfer
            local_send_phase_ms = $(if ($uploadSize -gt 0) { Delta-Ms $postTransfer (Get-Field $metric 'time_pretransfer') } else { $null })
            after_local_send_to_first_byte_ms = $(if ($uploadSize -gt 0 -and $null -ne $postTransfer -and [double](Get-Field $metric 'time_starttransfer' 0) -ge [double]$postTransfer) { Delta-Ms (Get-Field $metric 'time_starttransfer') $postTransfer } else { $null })
            first_byte_ms = Seconds-ToMs (Get-Field $metric 'time_starttransfer')
            response_receive_ms = $(if ([double](Get-Field $metric 'time_starttransfer' 0) -gt 0) { Delta-Ms (Get-Field $metric 'time_total') (Get-Field $metric 'time_starttransfer') } else { $null })
            total_ms = Seconds-ToMs (Get-Field $metric 'time_total')
            cpa_trace_id = Hide-Key $trace
            error = Hide-Key ([string](Get-Field $metric 'errormsg' ''))
            diagnostic_response = $bodySnippet
        }
        $results.Add($record)
        Write-Host ('  {0,-12} {1,-8} body={2,7}B HTTP={3} total={4}ms first={5}ms send={6}ms expected={7}' -f
            $record.route, $record.kind, $uploadSize, $status, $record.total_ms, $record.first_byte_ms, $record.local_send_phase_ms, $expected)
        # Save after every transfer so a partial run remains useful.
        Save-Report
    }
}

function Save-Report {
    $report.results = @($results.ToArray())
    [IO.File]::WriteAllText((Join-Path $OutputDirectory 'report.json'), ($report | ConvertTo-Json -Depth 12), $utf8)
    if ($results.Count -gt 0) { $results.ToArray() | Export-Csv -LiteralPath (Join-Path $OutputDirectory 'timings.csv') -NoTypeInformation -Encoding UTF8 }
}

function Median($Values) {
    $sorted = @($Values | Sort-Object)
    if ($sorted.Count -eq 0) { return $null }
    $middle = [int][math]::Floor($sorted.Count / 2)
    if ($sorted.Count % 2) { return [double]$sorted[$middle] }
    return ([double]$sorted[$middle - 1] + [double]$sorted[$middle]) / 2
}

try {
    $targetUri = [uri]$BaseUrl
    if (-not $targetUri.IsAbsoluteUri -or $targetUri.Scheme -notin @('http', 'https') -or
        $targetUri.UserInfo -or $targetUri.Query -or $targetUri.Fragment) { throw 'BaseUrl must be a plain http(s) API URL without credentials/query/fragment.' }
    $apiBase = $BaseUrl.TrimEnd('/')
    if ($apiBase -notmatch '/v1$') { $apiBase += '/v1' }
    if ($CompareIp) {
        $parsedIp = $null
        if (-not [Net.IPAddress]::TryParse($CompareIp, [ref]$parsedIp)) { throw 'CompareIp must be a numeric IPv4/IPv6 address.' }
    }
    $curlPath = (Get-Command curl.exe -ErrorAction Stop).Source
    $curlVersionText = (& $curlPath --disable --version | Select-Object -First 1)
    if ($curlVersionText -notmatch '^curl (\d+\.\d+\.\d+)') { throw 'Cannot detect curl version.' }
    $curlVersion = [version]$Matches[1]
    if ($curlVersion -lt [version]'7.75.0') { throw 'curl.exe 7.75+ is required; curl 8.10+ also reports local send completion.' }
    $supportsPostTransfer = $curlVersion -ge [version]'8.10.0'
    if (-not $OutputDirectory) {
        $OutputDirectory = Join-Path (Get-Location).Path ('portal-network-' + $runId)
    }
    $OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
    if (Test-Path -LiteralPath $OutputDirectory) { throw 'OutputDirectory already exists. Choose a new directory to preserve earlier reports.' }
    [void][IO.Directory]::CreateDirectory($OutputDirectory)
    [void][IO.Directory]::CreateDirectory($temporaryDirectory)
    if ($env:PORTAL_DIAG_API_KEY) {
        $apiKey = $env:PORTAL_DIAG_API_KEY.Trim()
    } elseif (-not $SkipUpload -and -not $NoPrompt) {
        Write-Host 'Enter your portal API Key (hidden; not saved in the report).'
        $secureKey = Read-Host 'API Key' -AsSecureString
        $keyPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
        try { $apiKey = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($keyPointer).Trim() }
        finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($keyPointer); $secureKey.Dispose() }
    }
    if ($apiKey -match '[\r\n\x00]') { throw 'Invalid API Key.' }
    Write-Host ('Run: {0} | label: {1} | target: {2}' -f $runId, $Label, $apiBase)
    Write-Host 'curl bypasses explicit HTTP proxies. A Clash/VPN TUN can still intercept traffic.'
    if (-not $SkipUpload -and $apiKey) {
        Write-Host ('Upload tests intentionally produce HTTP 400 for model {0}; no real inference.' -f $diagnosticModel)
        Write-Host 'These diagnostic requests appear in CPA access/error logs. They are not actual model failures.'
    } elseif (-not $SkipUpload) {
        Write-Host 'No API Key supplied: upload tests skipped. HTTP 401 checks reachability only.'
    }

    $dns = @()
    $dnsError = ''
    $dnsWatch = [Diagnostics.Stopwatch]::StartNew()
    try { $dns = @([Net.Dns]::GetHostAddresses($targetUri.DnsSafeHost) | ForEach-Object { $_.ToString() }) }
    catch { $dnsError = $_.Exception.Message }
    $dnsWatch.Stop()
    $proxyEnvironment = @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY') | ForEach-Object {
        [pscustomobject]@{ name = $_; configured = [bool][Environment]::GetEnvironmentVariable($_) }
    }
    $proxyProcesses = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -match 'clash|mihomo|sing-box|v2ray|xray|hysteria|tuic' } | Select-Object -ExpandProperty ProcessName -Unique)
    $adapters = @()
    try { $adapters = @(Get-NetIPInterface -ErrorAction Stop | Where-Object { $_.ConnectionState -eq 'Connected' } | Select-Object InterfaceAlias, AddressFamily, NlMtu, InterfaceMetric) } catch { }
    $systemProxyEnabled = $null
    try { $systemProxyEnabled = [bool](Get-ItemPropertyValue -LiteralPath 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -Name ProxyEnable) } catch { }
    $report = [pscustomobject][ordered]@{
        schema_version = 1; run_id = $runId; label = $Label; started_at_local = (Get-Date).ToString('o')
        base_url = $apiBase; compare_ip = $CompareIp; diagnostic_model = $diagnosticModel
        curl_version = $curlVersionText; timeout_seconds = $TimeoutSeconds; repeat = $Repeat
        supports_local_send_timing = $supportsPostTransfer; authenticated = [bool]$apiKey
        proxy_mode = 'explicit proxies bypassed; TUN/VPN not bypassed'
        proxy_environment = @($proxyEnvironment); system_proxy_enabled = $systemProxyEnabled
        possible_proxy_processes = $proxyProcesses; interfaces = $adapters
        dns = [pscustomobject]@{ addresses = $dns; elapsed_ms = [math]::Round($dnsWatch.Elapsed.TotalMilliseconds, 3); error = $dnsError }
        tcp_probes = @(); icmp_probes = @(); results = @()
    }
    Save-Report
    Write-Host ('DNS: ' + ($dns -join ', '))
    if ($proxyProcesses.Count) { Write-Host ('Possible proxy processes: ' + ($proxyProcesses -join ', ')) }

    $probeTargets = @($dns | Select-Object -First 2)
    if ($CompareIp -and $CompareIp -notin $probeTargets) { $probeTargets += $CompareIp }
    foreach ($address in $probeTargets) {
        for ($n = 0; $n -lt 3; $n++) {
            $socket = New-Object Net.Sockets.TcpClient ([Net.IPAddress]::Parse($address).AddressFamily)
            $watch = [Diagnostics.Stopwatch]::StartNew()
            $success = $false; $tcpError = ''; $async = $null
            try {
                $async = $socket.BeginConnect([Net.IPAddress]::Parse($address), $targetUri.Port, $null, $null)
                if (-not $async.AsyncWaitHandle.WaitOne(3000)) { throw 'TCP connect timed out after 3 seconds.' }
                $socket.EndConnect($async); $success = $true
            } catch { $tcpError = $_.Exception.Message }
            finally { $socket.Close(); if ($null -ne $async) { $async.AsyncWaitHandle.Close() } }
            $watch.Stop()
            $report.tcp_probes += [pscustomobject]@{ ip = $address; port = $targetUri.Port; success = $success; elapsed_ms = [math]::Round($watch.Elapsed.TotalMilliseconds, 3); error = $tcpError }
            $ping = New-Object Net.NetworkInformation.Ping
            try {
                $reply = $ping.Send($address, 1000)
                $report.icmp_probes += [pscustomobject]@{ ip = $address; status = $reply.Status.ToString(); rtt_ms = $reply.RoundtripTime }
            } catch { $report.icmp_probes += [pscustomobject]@{ ip = $address; status = 'error'; rtt_ms = $null } }
            finally { $ping.Dispose() }
        }
    }
    Save-Report

    $routes = @('dns')
    if ($CompareIp) { $routes += 'fixed-ip' }
    $index = 0
    foreach ($route in $routes) {
        Write-Host ('Testing route: ' + $route)
        for ($n = 0; $n -lt $Repeat; $n++) {
            $index++; Invoke-Curl @(Curl-Block 'small-get' $route '' $index)
        }
        # One curl process, sequential GETs: verify connection reuse, not long SSE stability.
        $reuseBlocks = @()
        for ($n = 0; $n -lt 3; $n++) { $index++; $reuseBlocks += Curl-Block 'reuse-get' $route '' $index }
        Invoke-Curl $reuseBlocks
        if (-not $SkipUpload -and $apiKey) {
            $authOkay = @($results.ToArray() | Where-Object { $_.route -eq $route -and $_.kind -ne 'upload' -and $_.http_status -eq 200 -and $_.curl_exit -eq 0 }).Count -gt 0
            if (-not $authOkay) { Write-Host 'Authenticated models GET did not succeed; skipping uploads on this route.'; continue }
            foreach ($length in ($UploadBytes | Select-Object -Unique)) {
                $payloadPath = New-Payload $length
                for ($n = 0; $n -lt $Repeat; $n++) {
                    $index++; Invoke-Curl @(Curl-Block 'upload' $route $payloadPath $index)
                }
            }
        }
    }

    if ($TraceRoute -and $probeTargets.Count) {
        Write-Host 'Collecting a bounded ICMP traceroute (missing hops do not establish TCP packet loss).'
        $traceInfo = New-Object Diagnostics.ProcessStartInfo
        $traceInfo.FileName = 'tracert.exe'
        $traceInfo.Arguments = '-d -h 12 -w 500 ' + $probeTargets[-1]
        $traceInfo.UseShellExecute = $false; $traceInfo.CreateNoWindow = $true; $traceInfo.RedirectStandardOutput = $true
        $traceProcess = New-Object Diagnostics.Process
        $traceProcess.StartInfo = $traceInfo
        try {
            [void]$traceProcess.Start()
            $traceTask = $traceProcess.StandardOutput.ReadToEndAsync()
            if (-not $traceProcess.WaitForExit(25000)) { $traceProcess.Kill(); $traceProcess.WaitForExit() }
            [IO.File]::WriteAllText((Join-Path $OutputDirectory 'traceroute.txt'), $traceTask.Result, $utf8)
        } finally { $traceProcess.Dispose() }
    }

    $summary = New-Object 'System.Collections.Generic.List[string]'
    $summary.Add('Portal network report: ' + $runId + ' | ' + $Label)
    $summary.Add('Target: ' + $apiBase)
    $summary.Add('Times are milliseconds. Each curl transfer is bounded by ' + $TimeoutSeconds + ' seconds.')
    $summary.Add('')
    foreach ($group in ($results.ToArray() | Group-Object route, kind, requested_bytes)) {
        $good = @($group.Group | Where-Object { $_.curl_exit -eq 0 -and $_.expected_response })
        $median = Median @($good | ForEach-Object { $_.total_ms })
        $summary.Add(('{0}: expected={1}/{2}, successful median total={3}ms' -f $group.Name, $good.Count, $group.Count, $median))
    }
    $summary.Add('')
    if (@($results.ToArray() | Where-Object { $_.curl_exit -ne 0 }).Count) {
        $summary.Add('Transport failures occurred. See curl_exit/error and whether any HTTP response arrived.')
        $summary.Add('Common curl exits: 6=DNS, 7=connect, 28=timeout, 35=TLS handshake, 60=certificate, 52=empty reply, 56=receive error.')
    }
    if (@($results.ToArray() | Where-Object { $_.kind -eq 'upload' -and $_.diagnostic_body_confirmed -and $_.total_ms -ge 5000 }).Count) {
        $summary.Add('At least one upload took >=5s without model inference. This reproduces a slow pre-inference path, but does not isolate ISP/router/portal/CPA by itself.')
    }
    if ($dns | Where-Object { $_ -match '^198\.(18|19)\.' }) {
        $summary.Add('DNS returned a 198.18/15 address, often used by proxy fake-IP. Compare the fixed-IP route; a TUN can still intercept it.')
    }
    $summary.Add('An expected diagnostic 400 mentioning the diagnostic model shows the application parsed that request; it is not a real model error.')
    $summary.Add('local_send_complete_ms is when libcurl sends its last byte, not when the server has received/ACKed the entire body.')
    $summary.Add('after_local_send_to_first_byte_ms includes queued network delivery, intermediaries, server processing and return traffic; it is not pure server wait.')
    $summary.Add('first_byte_ms/total_ms are cumulative; DNS/TCP/TLS/local_send_phase are stage durations. Failed transfers may have zero/unavailable timings.')
    $summary.Add('reuse-get checks sequential HTTP connection reuse only, not long-lived SSE or model output speed.')
    $summary.Add('ICMP failures/missing traceroute hops do not prove TCP loss. No packet capture or TCP retransmission count is collected.')
    $summary.Add('Compare the same command on the slow network and hotspot. Correlate run_id, local timestamps and cpa_trace_id with server logs for attribution.')
    $summary.Add('API Key, full request headers and actual chat contents are not saved. Reports do contain IPs, interface names and timestamps.')
    [IO.File]::WriteAllText((Join-Path $OutputDirectory 'summary.txt'), ($summary -join "`r`n"), $utf8)
    Save-Report
    Write-Host ''
    Write-Host ($summary -join "`n")
    Write-Host ('Saved reports: ' + $OutputDirectory)
} finally {
    $apiKey = ''
    # Only delete the generated immediate files; never recursively traverse a path.
    if (Test-Path -LiteralPath $temporaryDirectory) {
        Get-ChildItem -LiteralPath $temporaryDirectory -File | ForEach-Object { [IO.File]::Delete($_.FullName) }
        [IO.Directory]::Delete($temporaryDirectory)
    }
}
