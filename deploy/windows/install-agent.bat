@echo off
setlocal
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install-agent.ps1" %*
set "INSTALL_RESULT=%ERRORLEVEL%"
if not "%INSTALL_RESULT%"=="0" echo Installation failed. Review the error above.
pause
exit /b %INSTALL_RESULT%
