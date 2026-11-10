#Requires -Version 5.1
<#
.SYNOPSIS
  Download public model settings and apply them directly to Codex.
.DESCRIPTION
  With no parameters, preview changes and ask before applying. No portal login.
  Downloaded scripts include the portal address. Close Codex before applying.
#>
[CmdletBinding()]
param(
    [string]$PortalUrl = '', # PORTAL_URL_DEFAULT
    [string]$CodexHome,
    [switch]$UpdateApiKey,
    [switch]$Apply,
    [switch]$Interactive
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Stop'

function Get-Field($Object, [string]$Name, $Fallback = $null) {
    if ($null -eq $Object) { return $Fallback }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $Fallback }
    return $property.Value
}
function Set-Field($Object, [string]$Name, $Value) {
    $Object | Add-Member -MemberType NoteProperty -Name $Name -Value $Value -Force
}
function Read-Object([string]$Text, [string]$Label) {
    try { $value = ConvertFrom-Json -InputObject $Text -ErrorAction Stop }
    catch { throw ($Label + '不是有效 JSON。') }
    if ($null -eq $value -or $value -isnot [pscustomobject]) { throw ($Label + '格式无效。') }
    return $value
}
function Read-Yes([string]$Prompt, [bool]$DefaultYes = $false) {
    while ($true) {
        $answer = (Read-Host $Prompt).Trim().ToLowerInvariant()
        if ($answer -eq '') { return $DefaultYes }
        if ($answer -eq 'n') { return $false }
        if ($answer -eq 'y') { return $true }
        if ($DefaultYes) { Write-Host '回车应用；输入 n 取消。' }
        else { Write-Host '请输入 y 或 n；回车为 n。' }
    }
}
function Assert-URL([string]$URL, [bool]$RootOnly = $false) {
    $parsed = $null
    if (-not [Uri]::TryCreate($URL, [UriKind]::Absolute, [ref]$parsed) -or
        $parsed.UserInfo -or $parsed.Query -or $parsed.Fragment -or
        ($RootOnly -and $parsed.AbsolutePath -ne '/') -or
        ($parsed.Scheme -ne 'https' -and -not ($parsed.Scheme -eq 'http' -and $parsed.IsLoopback))) {
        throw '地址无效，远程服务必须使用 HTTPS，只有本机地址允许 HTTP。'
    }
}
function Get-PublicConfig([string]$Base) {
    Add-Type -AssemblyName System.Net.Http
    $handler = [Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect = $false
    $handler.UseCookies = $false
    $client = [Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(30)
    $response = $null
    try {
        $response = $client.GetAsync($Base.TrimEnd('/') + '/api/client-model-config', [Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
        $null = $response.Content.LoadIntoBufferAsync(4MB).GetAwaiter().GetResult()
        $body = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
        if ([int]$response.StatusCode -ne 200) {
            if ([int]$response.StatusCode -eq 404) { throw '管理员尚未设置客户端模型。' }
            throw ('获取模型配置失败，HTTP ' + [int]$response.StatusCode + '。')
        }
        $config = Read-Object $body '门户响应'
        if ((Get-Field $config 'version') -ne 2) { throw '门户配置版本不兼容，请重新下载脚本。' }
        $models = @(Get-Field $config 'models' @())
        if ($models.Count -lt 1 -or $models.Count -gt 128) { throw '模型数量无效。' }
        $ids = @{}
        foreach ($model in $models) {
            $id = [string](Get-Field $model 'id' '')
            if ($id -cnotmatch '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$' -or $ids.ContainsKey($id)) { throw '模型名称无效或重复。' }
            $ids[$id] = $true
            $effort = [string](Get-Field $model 'default_reasoning' '')
            if ($effort -and $effort -cnotin @('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra')) { throw '默认思考强度无效。' }
            $declared = @(Get-Field $model 'supported_reasoning_levels' @())
            if ($declared.Count -gt 8) { throw '上游思考档位数量无效。' }
            foreach ($level in $declared) {
                if ($level -isnot [string] -or $level -cnotin @('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra')) { throw '上游思考档位格式无效。' }
            }
        }
        if (-not $ids.ContainsKey([string](Get-Field $config 'default_model' ''))) { throw '默认模型不在模型列表中。' }
        Assert-URL ([string](Get-Field $config 'api_base_url' ''))
        return $config
    } finally {
        if ($null -ne $response) { $response.Dispose() }
        $client.Dispose()
        $handler.Dispose()
    }
}

# Edit a small, explicitly owned subset of TOML. Unrecognized syntax stops the
# operation before any write; unrelated sections and comments stay in place.
function Assert-SimpleToml([string]$Text) {
    if ($Text.Contains('"""') -or $Text.Contains("'''")) { throw '配置含多行 TOML 字符串，暂不自动改写。' }
    if ($Text -match '(?m)^\s*(?:model_providers|model|model_provider|model_catalog_json|model_reasoning_effort|profile)\s*=\s*[\[{]') { throw '配置包含暂不支持的内联结构，请先整理配置。' }
    if ($Text -match '(?m)^\s*(?:"|'')(?:model|model_provider|model_catalog_json|model_reasoning_effort|profile|model_providers)(?:"|'')\s*=') { throw '配置使用带引号的目标字段，暂不自动改写。' }
    foreach ($line in ($Text -split '\r?\n')) {
        if ($line -match '^\s*\[\s*model_providers\s*\.\s*(?:cliproxy_portal|"cliproxy_portal"|''cliproxy_portal'')' -and
            $line -notmatch '^\s*\[model_providers\.cliproxy_portal\]\s*(?:#.*)?$') { throw '门户连接配置表格式暂不支持，请先整理配置。' }
        if ($line -match '^\s*(?:cliproxy_portal|"cliproxy_portal"|''cliproxy_portal'')\s*=') { throw '门户连接使用内联配置，暂不自动改写。' }
    }
}
function Root-String([string]$Text, [string]$Key) {
    $root = [regex]::Split($Text, '(?m)^\s*\[')[0]
    $pattern = '(?m)^\s*' + [regex]::Escape($Key) + '\s*=\s*("(?:\\.|[^"\r\n])*"|''[^''\r\n]*'')\s*(?:#.*)?$'
    $matches = [regex]::Matches($root, $pattern)
    if ($matches.Count -gt 1) { throw '配置包含重复的根字段。' }
    if (-not $matches.Count) { return '' }
    $literal = $matches[0].Groups[1].Value
    if ($literal.StartsWith("'")) { return $literal.Substring(1, $literal.Length - 2) }
    try { return [string](ConvertFrom-Json -InputObject $literal -ErrorAction Stop) }
    catch { throw '配置字符串格式暂不支持。' }
}
function Section-Body([string]$Text, [string[]]$Parts) {
    $owned = $false
    $lines = [Collections.Generic.List[string]]::new()
    $partsRegex = @($Parts | ForEach-Object { $p = [regex]::Escape($_); '(?:' + $p + '|"' + $p + '"|''' + $p + ''')' }) -join '\s*\.\s*'
    $headerRegex = '^\s*\[\s*' + $partsRegex + '\s*\]\s*(?:#.*)?$'
    foreach ($line in ($Text -split '\r?\n')) {
        if ($line -match '^\s*\[') {
            $owned = $line -match $headerRegex
            continue
        }
        if ($owned) { $lines.Add($line) }
    }
    return $lines -join "`n"
}
function Find-CurrentCredential([string]$Text, [string]$HomePath) {
    $provider = Root-String $Text 'model_provider'
    $profile = Root-String $Text 'profile'
    if ($profile) {
        $profileProvider = Root-String (Section-Body $Text @('profiles', $profile)) 'model_provider'
        if ($profileProvider) { $provider = $profileProvider }
    }
    if (-not $provider) { $provider = 'openai' }
    # Follow the active connection first, then our previously saved connection.
    foreach ($id in @($provider, 'cliproxy_portal') | Select-Object -Unique) {
        $body = Section-Body $Text @('model_providers', $id)
        $directKey = Root-String $body 'experimental_bearer_token'
        if ($directKey) { return [pscustomobject]@{ Key = $directKey; Environment = ''; Source = '当前连接配置' } }
        $environment = Root-String $body 'env_key'
        if ($environment) {
            foreach ($scope in @('Process', 'User', 'Machine')) {
                if ([Environment]::GetEnvironmentVariable($environment, $scope)) {
                    return [pscustomobject]@{ Key = ''; Environment = $environment; Source = '当前连接的环境变量' }
                }
            }
        }
    }
    # auth.json's API key is different from ChatGPT OAuth access/refresh tokens.
    # Never repurpose a login token as a third-party API key.
    $authPath = Join-Path $HomePath 'auth.json'
    if (Test-Path -LiteralPath $authPath -PathType Leaf) {
        if ((Get-Item -LiteralPath $authPath).Length -gt 1MB) { throw '认证文件过大，无法读取当前 API Key。' }
        $auth = Read-Object ([IO.File]::ReadAllText($authPath)) 'Codex 认证文件'
        $authKey = [string](Get-Field $auth 'OPENAI_API_KEY' '')
        if ($authKey) { return [pscustomobject]@{ Key = $authKey; Environment = ''; Source = 'Codex 当前 API Key' } }
    }
    return [pscustomobject]@{ Key = ''; Environment = ''; Source = '' }
}
function Build-Config([string]$Old, $Config, [string]$CatalogPath, [string]$Key, [string]$KeyEnvironment) {
    Assert-SimpleToml $Old
    $root = $true
    $owned = $false
    $lines = [Collections.Generic.List[string]]::new()
    foreach ($line in ($Old -split '\r?\n')) {
        if ($line -match '^\s*\[') {
            $root = $false
            $owned = $line -match '^\s*\[model_providers\.cliproxy_portal\]\s*(?:#.*)?$'
            if ($line -match '^\s*\[(?:model_providers\.(?:"cliproxy_portal"|''cliproxy_portal'')|model_providers\.cliproxy_portal\.|profiles\.)') {
                if ($line -notmatch '^\s*\[profiles\.') { throw '门户供应商包含复杂子表，请先整理配置。' }
            }
        }
        if ($owned) { continue }
        if ($root -and $line -match '^\s*(?:model|model_provider|model_reasoning_effort|model_catalog_json|profile)\s*=') { continue }
        $lines.Add($line)
    }
    $default = @($Config.models | Where-Object { $_.id -ceq $Config.default_model })[0]
    $head = 'model = ' + (ConvertTo-Json -InputObject $Config.default_model -Compress) + "`nmodel_provider = `"cliproxy_portal`"`n"
    if ($default.default_reasoning) { $head += 'model_reasoning_effort = ' + (ConvertTo-Json -InputObject $default.default_reasoning -Compress) + "`n" }
    $head += 'model_catalog_json = ' + (ConvertTo-Json -InputObject $CatalogPath -Compress) + "`n"
    $provider = "`n[model_providers.cliproxy_portal]`nname = `"CLIProxy Portal`"`nwire_api = `"responses`"`nrequires_openai_auth = false`nbase_url = " + (ConvertTo-Json -InputObject $Config.api_base_url -Compress) + "`n"
    if ($KeyEnvironment) { $provider += 'env_key = ' + (ConvertTo-Json -InputObject $KeyEnvironment -Compress) + "`n" }
    else { $provider += 'experimental_bearer_token = ' + (ConvertTo-Json -InputObject $Key -Compress) + "`n" }
    return $head + ($lines -join "`n").TrimEnd() + "`n" + $provider
}

function Complete-ModelInstructions($Entry) {
    $messages = Get-Field $Entry 'model_messages'
    $template = Get-Field $messages 'instructions_template'
    $instructions = Get-Field $Entry 'base_instructions'
    if ($instructions -isnot [string] -or [string]::IsNullOrWhiteSpace($instructions)) {
        if ($template -is [string] -and -not [string]::IsNullOrWhiteSpace($template)) {
            $instructions = $template
        } else {
            $instructions = "You are a coding assistant. Follow the user's request and applicable workspace instructions. Use available tools to inspect the workspace and complete the requested changes. Explain the result clearly and accurately."
        }
        Set-Field $Entry 'base_instructions' $instructions
    }
    # A malformed optional messages object would still fail deserialization even
    # with valid base instructions. Fall back to the complete base instructions.
    if ($null -ne $messages -and ($messages -isnot [pscustomobject] -or $template -isnot [string])) {
        Set-Field $Entry 'model_messages' $null
    }
}
function Assert-Catalog($Catalog, [int]$ExpectedCount) {
    $models = @(Get-Field $Catalog 'models' @())
    if ($models.Count -ne $ExpectedCount) { throw '生成的模型目录数量不一致，未修改文件。' }
    $seen = @{}
    foreach ($entry in $models) {
        $id = [string](Get-Field $entry 'slug' '')
        if (-not $id -or $seen.ContainsKey($id)) { throw '生成的模型目录名称缺失或重复，未修改文件。' }
        $seen[$id] = $true
        if ((Get-Field $entry 'base_instructions') -isnot [string] -or
            [string]::IsNullOrWhiteSpace([string]$entry.base_instructions)) {
            throw ('模型 ' + $id + ' 缺少有效指令字段，未修改文件。')
        }
        foreach ($field in @('display_name', 'description', 'shell_type', 'visibility')) {
            if ((Get-Field $entry $field) -isnot [string]) { throw ('模型 ' + $id + ' 的 ' + $field + ' 字段格式无效，未修改文件。') }
        }
        if ((Get-Field $entry 'supported_in_api') -isnot [bool]) { throw ('模型 ' + $id + ' 的可用性字段格式无效，未修改文件。') }
        $effort = Get-Field $entry 'default_reasoning_level'
        $levels = @(Get-Field $entry 'supported_reasoning_levels' @())
        foreach ($level in $levels) {
            if ((Get-Field $level 'effort') -isnot [string] -or (Get-Field $level 'description') -isnot [string]) {
                throw ('模型 ' + $id + ' 的思考档位格式无效，未修改文件。')
            }
        }
        if ($effort -and @($levels | Where-Object { (Get-Field $_ 'effort' '') -ceq $effort }).Count -eq 0) {
            throw ('模型 ' + $id + ' 的默认思考强度不在档位列表中，未修改文件。')
        }
    }
}
function Set-DeclaredReasoningLevels($Entry, $Declared) {
    $byEffort = @{}
    foreach ($level in @($Declared)) {
        if (-not $byEffort.ContainsKey([string]$level)) {
            $byEffort[[string]$level] = [pscustomobject]@{ effort = [string]$level; description = [string]$level }
        }
    }
    $all = @()
    foreach ($level in @('none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max', 'ultra')) {
        if ($byEffort.ContainsKey($level)) { $all += $byEffort[$level] }
    }
    Set-Field $Entry 'supported_reasoning_levels' $all
    Set-Field $Entry 'default_reasoning_level' $null
    $Entry.PSObject.Properties.Remove('portal_sync_original')
}
function Build-Catalog($Config, [string]$HomePath, [string]$OldToml) {
    $known = @{}
    $paths = [Collections.Generic.List[string]]::new()
    $previous = Root-String $OldToml 'model_catalog_json'
    if ($previous) {
        if (-not [IO.Path]::IsPathRooted($previous)) { $previous = Join-Path $HomePath $previous }
        $paths.Add($previous)
    }
    # Fresh native metadata takes precedence over our previous default overrides.
    $paths.Add((Join-Path $HomePath 'models_cache.json'))
    foreach ($path in $paths) {
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { continue }
        if ((Get-Item -LiteralPath $path).Length -gt 16MB) { continue }
        try {
            $source = Read-Object ([IO.File]::ReadAllText($path)) '本地模型目录'
            foreach ($entry in @(Get-Field $source 'models' @())) {
                $slug = [string](Get-Field $entry 'slug' '')
                if ($slug) {
                    $known[$slug] = $entry
                }
            }
        } catch { Write-Warning '本地模型目录无法读取，将使用基础模型描述。' }
    }
    $entries = [Collections.Generic.List[object]]::new()
    $ordered = @($Config.models | Where-Object { $_.id -ceq $Config.default_model }) + @($Config.models | Where-Object { $_.id -cne $Config.default_model })
    foreach ($model in $ordered) {
        $id = [string]$model.id
        if ($known.ContainsKey($id)) {
            $entry = Read-Object (ConvertTo-Json -InputObject $known[$id] -Depth 100 -Compress -WarningAction Stop) '模型元数据'
        } else {
            # Conservative schema-complete descriptor for custom aliases. Do not
            # borrow another model's instructions, tool capability or token limit.
            $entry = [pscustomobject]@{
                slug = $id; display_name = $id; description = $id
                default_reasoning_level = $null; supported_reasoning_levels = @()
                shell_type = 'shell_command'; visibility = 'list'; supported_in_api = $true
                priority = 0; availability_nux = $null; upgrade = $null
                model_messages = $null
                supports_reasoning_summaries = $false; default_reasoning_summary = 'none'
                support_verbosity = $false; default_verbosity = $null
                apply_patch_tool_type = 'freeform'; truncation_policy = @{ mode = 'tokens'; limit = 10000 }
                supports_parallel_tool_calls = $false; context_window = 128000
                effective_context_window_percent = 95; experimental_supported_tools = @()
                input_modalities = @('text'); use_responses_lite = $false
                portal_sync_fallback = $true
            }
        }
        Complete-ModelInstructions $entry
        Set-DeclaredReasoningLevels $entry @(Get-Field $model 'supported_reasoning_levels' @())
        Set-Field $entry 'visibility' 'list'
        Set-Field $entry 'supported_in_api' $true
        Set-Field $entry 'priority' $entries.Count
        Set-Field $entry 'use_responses_lite' $false
        # AGY supports client-executed tool discovery and image content. Migrate
        # stale local entries so Codex can advertise those client capabilities.
        if ($id.StartsWith('agy/', [StringComparison]::OrdinalIgnoreCase)) {
            Set-Field $entry 'supports_search_tool' $true
            # The bridge forwards embedded images as ACP image content blocks.
            # Override stale text-only fallback entries as well as new entries.
            Set-Field $entry 'input_modalities' @('text', 'image')
        }
        $effort = [string]$model.default_reasoning
        if ($effort) {
            Set-Field $entry 'default_reasoning_level' $effort
            $levels = @(Get-Field $entry 'supported_reasoning_levels' @())
            if (@($levels | Where-Object { (Get-Field $_ 'effort' '') -ceq $effort }).Count -eq 0) {
                # A default is never used to shrink the capability list.
                throw ('模型 ' + $id + ' 的上游声明未包含默认思考强度 ' + $effort + '，请刷新配置或调整默认值。')
            }
        }
        $entries.Add($entry)
    }
    $catalog = [pscustomobject]@{ models = @($entries.ToArray()) }
    $expectedCount = @($Config.models).Count
    Assert-Catalog $catalog $expectedCount
    $json = ConvertTo-Json -InputObject $catalog -Depth 100 -WarningAction Stop
    # Validate the serialized document that will actually be written.
    Assert-Catalog (Read-Object $json '生成的模型目录') $expectedCount
    return $json
}

function Assert-CodexClosed {
    $running = @(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -match '^(Codex|codex-app|codex-desktop|codex_plus_manager|codex-plus-manager|codex-plus-launcher)$' })
    if ($running.Count) { throw '请先完全退出 Codex 和正在运行的配置管理器，再应用。脚本不会结束进程。' }
}
function File-Hash([string]$Path) {
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $item = Get-Item -LiteralPath $Path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint -or $item.PSIsContainer) { throw '配置目标不是普通文件，已停止写入。' }
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
}
function Secure-File([string]$Path) {
    if ($env:OS -ne 'Windows_NT') { throw '此脚本仅支持 Windows。' }
    $acl = [Security.AccessControl.FileSecurity]::new()
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in @([Security.Principal.WindowsIdentity]::GetCurrent().User, [Security.Principal.SecurityIdentifier]::new('S-1-5-18'), [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'))) {
        $rule = [Security.AccessControl.FileSystemAccessRule]::new($sid, [Security.AccessControl.FileSystemRights]::FullControl, [Security.AccessControl.AccessControlType]::Allow)
        $null = $acl.AddAccessRule($rule)
    }
    # Only access rules were modified. Avoid PowerShell's Set-Acl provider,
    # which may request audit/SACL access and require SeSecurityPrivilege.
    if ($PSVersionTable.PSVersion.Major -le 5) {
        [IO.File]::SetAccessControl($Path, $acl)
    } else {
        [IO.FileSystemAclExtensions]::SetAccessControl([IO.FileInfo]::new($Path), $acl)
    }
}
function Write-Transaction($Plans) {
    Assert-CodexClosed
    $stamp = (Get-Date -Format 'yyyyMMdd-HHmmss') + '-' + [Guid]::NewGuid().ToString('N').Substring(0, 8)
    $written = [Collections.Generic.List[object]]::new()
    try {
        foreach ($plan in $Plans) {
            if ((File-Hash $plan.Path) -cne $plan.Hash) { throw '配置在预览后发生变化，请重新运行。' }
            $parent = [IO.Path]::GetDirectoryName($plan.Path)
            if (-not (Test-Path -LiteralPath $parent)) { $null = New-Item -ItemType Directory -Path $parent }
            $plan.Temp = $plan.Path + '.portal-sync-' + $stamp + '.tmp'
            $plan.Backup = $plan.Path + '.portal-sync-' + $stamp + '.bak'
            $stream = [IO.File]::Open($plan.Temp, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
            $stream.Dispose()
            Secure-File $plan.Temp
            [IO.File]::WriteAllText($plan.Temp, $plan.Text + "`n", [Text.UTF8Encoding]::new($false))
            $plan.WrittenHash = File-Hash $plan.Temp
        }
        foreach ($plan in $Plans) {
            Assert-CodexClosed
            if ((File-Hash $plan.Path) -cne $plan.Hash) { throw '应用时配置发生变化，已停止。' }
            if ($plan.Hash) { [IO.File]::Replace($plan.Temp, $plan.Path, $plan.Backup) }
            else { [IO.File]::Move($plan.Temp, $plan.Path) }
            $written.Add($plan)
            if ((File-Hash $plan.Path) -cne $plan.WrittenHash) { throw '文件写入后校验失败。' }
            if ($plan.Hash) { Secure-File $plan.Backup }
            Secure-File $plan.Path
        }
    } catch {
        foreach ($plan in @($written.ToArray()) | Sort-Object Order -Descending) {
            if ((File-Hash $plan.Path) -cne $plan.WrittenHash) { Write-Warning ('文件被外部程序修改，请从备份手动恢复：' + $plan.Backup); continue }
            if ($plan.Hash) { [IO.File]::Replace($plan.Backup, $plan.Path, $null) }
            else { Remove-Item -LiteralPath $plan.Path -Force }
        }
        throw
    } finally {
        foreach ($plan in $Plans) {
            if ($plan.Temp -and (Test-Path -LiteralPath $plan.Temp)) { Remove-Item -LiteralPath $plan.Temp -Force }
        }
    }
    foreach ($plan in $written) {
        Write-Host ('已应用：' + $plan.Path)
        if ($plan.Hash) { Write-Host ('原文件备份：' + $plan.Backup) }
    }
}

$apiKey = ''
$secureKey = $null
$keyPointer = [IntPtr]::Zero
try {
    if ($PSBoundParameters.Count -eq 0) { $Interactive = $true }
    if (-not $CodexHome) { $CodexHome = $env:CODEX_HOME }
    if (-not $CodexHome) { $CodexHome = Join-Path ([Environment]::GetFolderPath('UserProfile')) '.codex' }
    $homePath = [IO.Path]::GetFullPath($CodexHome)
    $ancestor = $homePath
    while ($ancestor) {
        if (Test-Path -LiteralPath $ancestor) {
            if ((Get-Item -LiteralPath $ancestor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) { throw '配置目录经过符号链接，请用 -CodexHome 明确指定实际目录。' }
        }
        $ancestor = [IO.Path]::GetDirectoryName($ancestor)
    }
    Assert-URL $PortalUrl $true
    $path = Join-Path $homePath 'config.toml'
    $catalogPath = Join-Path $homePath 'portal-models.json'
    $oldHash = File-Hash $path
    $catalogHash = File-Hash $catalogPath
    $old = ''
    if ($oldHash) {
        if ((Get-Item -LiteralPath $path).Length -gt 4MB) { throw '配置文件过大，已停止。' }
        $old = [IO.File]::ReadAllText($path)
    }
    Assert-SimpleToml $old
    Write-Host 'Codex 配置同步'
    Write-Host ('门户地址：' + $PortalUrl)
    Write-Host ('配置目录：' + $homePath)
    $config = Get-PublicConfig $PortalUrl
    $credential = Find-CurrentCredential $old $homePath
    $apiKey = $credential.Key
    $keyEnvironment = $credential.Environment
    if ($apiKey -or $keyEnvironment) { Write-Host ('已找到可沿用的 API Key：' + $credential.Source + '（不回显）。') }
    if ($Interactive -and -not $UpdateApiKey -and ($apiKey -or $keyEnvironment)) { $UpdateApiKey = Read-Yes '是否更新 API Key？y / n（回车沿用原 Key）' }
    if ($UpdateApiKey -or (-not $apiKey -and -not $keyEnvironment)) {
        $prompt = '请输入 API Key（隐藏输入）'
        if ($apiKey -or $keyEnvironment) { $prompt = '输入新 API Key（隐藏输入，回车保留）' }
        $secureKey = Read-Host $prompt -AsSecureString
        if ($secureKey.Length -gt 0) {
            $keyPointer = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secureKey)
            $apiKey = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($keyPointer).Trim()
            if ($apiKey -cnotmatch '^[\x21-\x7E]{8,4096}$') { throw 'API Key 格式无效。' }
            $keyEnvironment = ''
        }
    }
    if (-not $apiKey -and -not $keyEnvironment) { throw '尚未填写 API Key，未修改配置。' }
    $catalogText = Build-Catalog $config $homePath $old
    $newToml = Build-Config $old $config $catalogPath $apiKey $keyEnvironment
    $default = @($config.models | Where-Object { $_.id -ceq $config.default_model })[0]
    Write-Host ('API 地址：' + $config.api_base_url)
    Write-Host ('默认模型：' + $config.default_model + '；默认思考强度：' + $(if ($default.default_reasoning) { $default.default_reasoning } else { '沿用模型默认' }))
    Write-Host ('模型数量：' + @($config.models).Count)
    foreach ($model in $config.models) { Write-Host ('  ' + $model.id + '：' + $(if ($model.default_reasoning) { $model.default_reasoning } else { '沿用模型默认' })) }
    Write-Host 'API Key 不回显。应用前请完全退出 Codex，应用后重新打开并新建聊天。'
    $changed = $old.TrimEnd() -cne $newToml.TrimEnd()
    if (-not $catalogHash -or [IO.File]::ReadAllText($catalogPath).TrimEnd() -cne $catalogText.TrimEnd()) { $changed = $true }
    if ($changed -and $Interactive -and -not $Apply) { $Apply = Read-Yes '是否备份并应用以上配置？y / n（回车应用）' $true }
    if (-not $changed) { Write-Host '配置已一致，无需写入。' }
    elseif (-not $Apply) {
        if ($Interactive) { Write-Host '已取消，未修改文件。' }
        else { Write-Host '仅预览，未修改文件。添加 -Apply 参数可应用。' }
    }
    else {
        $plans = @(
            [pscustomobject]@{ Order = 0; Path = $catalogPath; Hash = $catalogHash; Text = $catalogText; Temp = ''; Backup = ''; WrittenHash = '' },
            [pscustomobject]@{ Order = 1; Path = $path; Hash = $oldHash; Text = $newToml; Temp = ''; Backup = ''; WrittenHash = '' }
        )
        Write-Transaction $plans
        Write-Host '模型目录、默认模型和默认思考强度已直接应用。请重新打开 Codex；已有聊天的手动选择保留。'
    }
} catch {
    Write-Error -Message ('同步失败：' + $_.Exception.Message) -ErrorAction Continue
    exit 1
} finally {
    $apiKey = $null
    if ($keyPointer -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($keyPointer) }
    if ($null -ne $secureKey) { $secureKey.Dispose() }
    $credential = $null
    if ($Interactive) { $null = Read-Host '按回车键关闭窗口' }
}
