Set WshShell = CreateObject("WScript.Shell")
WshShell.CurrentDirectory = "d:\code\anycode\daemon-go"
WshShell.Run "cmd /c .\anycode-daemon.exe --port 9527 > daemon.log 2>&1", 0, False
