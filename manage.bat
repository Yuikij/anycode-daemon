@echo off
cd /d "%~dp0"
:menu
cls
echo =========================================
echo       AnyCode Daemon Management Menu
echo =========================================
echo.
echo  1. Start Daemon (Background)
echo  2. Stop Daemon
echo  3. Check Status
echo  4. View Logs (Opens in Notepad)
echo  5. Update Daemon (git pull ^& go build)
echo  6. Setup Auto-start on Boot
echo  7. Show QR Code (for mobile connection)
echo  0. Exit
echo.
set /p choice="Enter your choice (0-7): "

if "%choice%"=="1" goto start
if "%choice%"=="2" goto stop
if "%choice%"=="3" goto status
if "%choice%"=="4" goto logs
if "%choice%"=="5" goto update
if "%choice%"=="6" goto setup
if "%choice%"=="7" goto qrcode
if "%choice%"=="0" exit
goto menu

:start
echo Starting Daemon in the background...
wscript.exe start-hidden.vbs
echo Start command sent!
pause
goto menu

:stop
echo Stopping Daemon...
taskkill /F /IM anycode-daemon.exe /T
echo Stop command sent!
pause
goto menu

:status
tasklist | findstr anycode-daemon.exe
if errorlevel 1 (
    echo Daemon is NOT running.
) else (
    echo Daemon is running!
)
pause
goto menu

:logs
if exist daemon.log (
    echo Opening daemon.log in Notepad...
    start notepad.exe daemon.log
) else (
    echo daemon.log does not exist yet.
)
pause
goto menu

:update
echo =========================================
echo Updating AnyCode Daemon...
echo 1. Stopping running Daemon
taskkill /F /IM anycode-daemon.exe /T >nul 2>&1
echo 2. Pulling latest code (git pull)
git pull
echo 3. Building new anycode-daemon.exe
go build -o anycode-daemon.exe .
echo Update complete! You can start it now.
echo =========================================
pause
goto menu

:setup
echo Setting up Auto-start...
powershell -ExecutionPolicy Bypass -File setup-startup.ps1
pause
goto menu

:qrcode
if not exist daemon.log (
    echo daemon.log does not exist! Please start the daemon first.
    pause
    goto menu
)
echo Checking IP and Token to generate direct-connect deep link...
powershell -ExecutionPolicy Bypass -File show-qr.ps1
pause
goto menu
