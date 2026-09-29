@echo off
cd /d "%~dp0"
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0Test-PortalNetwork.ps1" %*
set "diagnostic_exit=%ERRORLEVEL%"
echo.
echo Diagnostic finished. Exit code: %diagnostic_exit%
pause
exit /b %diagnostic_exit%
