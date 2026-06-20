$targetDir = "d:\code\anycode\daemon-go"
$vbsPath = "$targetDir\start-hidden.vbs"
$shortcutPath = "$env:APPDATA\Microsoft\Windows\Start Menu\Programs\Startup\AnyCodeDaemon.lnk"

$vbsContent = "Set WshShell = CreateObject(`"WScript.Shell`")`r`nWshShell.CurrentDirectory = `"$targetDir`"`r`nWshShell.Run `"cmd /c .\anycode-daemon.exe --port 9527 > daemon.log 2>&1`", 0, False"

[System.IO.File]::WriteAllText($vbsPath, $vbsContent, [System.Text.Encoding]::ASCII)

$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut($shortcutPath)
$Shortcut.TargetPath = "wscript.exe"
$Shortcut.Arguments = "`"$vbsPath`""
$Shortcut.WorkingDirectory = $targetDir
$Shortcut.Description = "AnyCode Daemon Auto-start"
$Shortcut.Save()

Write-Host "Startup shortcut created successfully."
Write-Host "Target: $shortcutPath"
Write-Host "VBS Path: $vbsPath"
