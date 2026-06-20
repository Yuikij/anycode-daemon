@echo off
REM Local dev launcher that bypasses the buggy cursor-agent version-dir shim.
REM Runs the bundled node + index.js directly. Used via ANYCODE_CURSOR_BIN.
set "CURSOR_AGENT_VER=%LOCALAPPDATA%\cursor-agent\versions\2026.06.15-18-00-12-6f5a2cf"
"%CURSOR_AGENT_VER%\node.exe" "%CURSOR_AGENT_VER%\index.js" %*
