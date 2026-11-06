@echo off
setlocal DisableDelayedExpansion
set "CODEX_PORTAL_SYNC_SOURCE=%~f0"
set "CODEX_PORTAL_SYNC_ENGINE=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
if not exist "%CODEX_PORTAL_SYNC_ENGINE%" goto missing_powershell
if not defined TEMP goto prepare_failed

:prepare
set "CODEX_PORTAL_SYNC_DIR=%TEMP%\codex-portal-sync-%RANDOM%-%RANDOM%"
if exist "%CODEX_PORTAL_SYNC_DIR%" goto prepare
mkdir "%CODEX_PORTAL_SYNC_DIR%" >nul 2>&1
if errorlevel 1 goto prepare_failed
"%CODEX_PORTAL_SYNC_ENGINE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; try { $text=[IO.File]::ReadAllText($env:CODEX_PORTAL_SYNC_SOURCE); $marker=':: CODEX_PORTAL_SYNC_PAYLOAD'; $offset=$text.LastIndexOf($marker,[StringComparison]::Ordinal); if ($offset -lt 0) { throw 'Payload missing' }; $bytes=[Convert]::FromBase64String($text.Substring($offset+$marker.Length)); $source=[Text.UTF8Encoding]::new($false,$true).GetString($bytes).TrimStart([char]0xfeff); $path=Join-Path $env:CODEX_PORTAL_SYNC_DIR 'Sync-Codex.ps1'; [IO.File]::WriteAllText($path,$source,[Text.UTF8Encoding]::new($true)); $tokens=$null; $syntaxErrors=$null; $null=[Management.Automation.Language.Parser]::ParseFile($path,[ref]$tokens,[ref]$syntaxErrors); if ($syntaxErrors.Count -gt 0) { throw $syntaxErrors[0].Message } } catch { [Console]::Error.WriteLine('Unable to prepare the sync script: '+$_.Exception.Message); exit 1 }"
if errorlevel 1 goto extraction_failed
"%CODEX_PORTAL_SYNC_ENGINE%" -NoLogo -NoProfile -ExecutionPolicy Bypass -File "%CODEX_PORTAL_SYNC_DIR%\Sync-Codex.ps1" %*
set "CODEX_PORTAL_SYNC_EXIT=%ERRORLEVEL%"
goto cleanup

:extraction_failed
set "CODEX_PORTAL_SYNC_EXIT=1"
echo Unable to start Codex configuration sync.
pause

:cleanup
del /f /q "%CODEX_PORTAL_SYNC_DIR%\Sync-Codex.ps1" >nul 2>&1
rmdir "%CODEX_PORTAL_SYNC_DIR%" >nul 2>&1
exit /b %CODEX_PORTAL_SYNC_EXIT%

:missing_powershell
echo Windows PowerShell 5.1 is required to run this script.
pause
exit /b 1

:prepare_failed
echo Unable to create a temporary directory for Codex configuration sync.
pause
exit /b 1

:: CODEX_PORTAL_SYNC_PAYLOAD
