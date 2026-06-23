package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func cmdInstall(args []string) {
	fmt.Println("Installing AnyCode Daemon auto-start service...")

	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("Error getting executable path: %v\n", err)
		os.Exit(1)
	}
	exePath, err = filepath.Abs(exePath)
	if err != nil {
		fmt.Printf("Error parsing executable path: %v\n", err)
		os.Exit(1)
	}

	switch runtime.GOOS {
	case "windows":
		installWindows(exePath)
	case "darwin":
		installDarwin(exePath)
	case "linux":
		installLinux(exePath)
	default:
		fmt.Printf("Auto-start installation is not natively supported on %s.\n", runtime.GOOS)
		os.Exit(1)
	}
}

func cmdUninstall(args []string) {
	fmt.Println("Uninstalling AnyCode Daemon auto-start service...")

	switch runtime.GOOS {
	case "windows":
		uninstallWindows()
	case "darwin":
		uninstallDarwin()
	case "linux":
		uninstallLinux()
	default:
		fmt.Printf("Auto-start uninstallation is not natively supported on %s.\n", runtime.GOOS)
		os.Exit(1)
	}
}

func installWindows(exePath string) {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		fmt.Println("Could not find APPDATA environment variable.")
		os.Exit(1)
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		homeDir = filepath.Dir(appData)
	}

	anycodeDir := filepath.Join(homeDir, ".anycode")
	os.MkdirAll(anycodeDir, 0755)

	vbsPath := filepath.Join(anycodeDir, "start-hidden.vbs")
	vbsContent := fmt.Sprintf(`Set WshShell = CreateObject("WScript.Shell")
WshShell.CurrentDirectory = "%s"
WshShell.Run "cmd /c """"& Chr(34) & "%s" & Chr(34) & """ start > ""%s\daemon.log"" 2>&1", 0, False
`, filepath.Dir(exePath), exePath, anycodeDir)

	if err := os.WriteFile(vbsPath, []byte(vbsContent), 0644); err != nil {
		fmt.Printf("Failed to write VBS script: %v\n", err)
		os.Exit(1)
	}

	shortcutPath := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "AnyCodeDaemon.lnk")

	psScript := fmt.Sprintf(`$WshShell = New-Object -ComObject WScript.Shell
$Shortcut = $WshShell.CreateShortcut("%s")
$Shortcut.TargetPath = "wscript.exe"
$Shortcut.Arguments = "`+"`"+`"%s`+"`"+`""
$Shortcut.WorkingDirectory = "%s"
$Shortcut.Description = "AnyCode Daemon Auto-start"
$Shortcut.Save()`, shortcutPath, vbsPath, filepath.Dir(exePath))

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psScript)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to create shortcut using PowerShell: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Successfully installed AnyCode Daemon to Windows Startup.")
	fmt.Printf("   Shortcut: %s\n", shortcutPath)
}

func uninstallWindows() {
	appData := os.Getenv("APPDATA")
	if appData != "" {
		shortcutPath := filepath.Join(appData, "Microsoft", "Windows", "Start Menu", "Programs", "Startup", "AnyCodeDaemon.lnk")
		if err := os.Remove(shortcutPath); err == nil {
			fmt.Println("Removed startup shortcut.")
		} else if !os.IsNotExist(err) {
			fmt.Printf("Failed to remove shortcut: %v\n", err)
		}
	}
	fmt.Println("✅ Successfully uninstalled AnyCode Daemon from Windows Startup.")
}

func installDarwin(exePath string) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		fmt.Println("Could not find user home directory.")
		os.Exit(1)
	}

	plistDir := filepath.Join(homeDir, "Library", "LaunchAgents")
	os.MkdirAll(plistDir, 0755)

	plistPath := filepath.Join(plistDir, "com.anycode.daemon.plist")

	anycodeDir := filepath.Join(homeDir, ".anycode")
	os.MkdirAll(anycodeDir, 0755)
	logFile := filepath.Join(anycodeDir, "daemon.log")

	plistContent := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.anycode.daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>start</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>WorkingDirectory</key>
    <string>%s</string>
</dict>
</plist>
`, exePath, logFile, logFile, filepath.Dir(exePath))

	if err := os.WriteFile(plistPath, []byte(plistContent), 0644); err != nil {
		fmt.Printf("Failed to write LaunchAgent plist: %v\n", err)
		os.Exit(1)
	}

	exec.Command("launchctl", "unload", plistPath).Run()

	cmd := exec.Command("launchctl", "load", "-w", plistPath)
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to load LaunchAgent: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Successfully installed and started AnyCode Daemon as a macOS LaunchAgent.")
}

func uninstallDarwin() {
	homeDir, _ := os.UserHomeDir()
	plistPath := filepath.Join(homeDir, "Library", "LaunchAgents", "com.anycode.daemon.plist")

	exec.Command("launchctl", "unload", "-w", plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Failed to remove plist: %v\n", err)
	}

	fmt.Println("✅ Successfully uninstalled AnyCode Daemon LaunchAgent.")
}

func installLinux(exePath string) {
	homeDir, _ := os.UserHomeDir()
	if homeDir == "" {
		fmt.Println("Could not find user home directory.")
		os.Exit(1)
	}

	systemdDir := filepath.Join(homeDir, ".config", "systemd", "user")
	os.MkdirAll(systemdDir, 0755)

	servicePath := filepath.Join(systemdDir, "anycode-daemon.service")

	serviceContent := fmt.Sprintf(`[Unit]
Description=AnyCode Daemon
After=network.target

[Service]
Type=simple
ExecStart=%s start
WorkingDirectory=%s
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
`, exePath, filepath.Dir(exePath))

	if err := os.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		fmt.Printf("Failed to write systemd service file: %v\n", err)
		os.Exit(1)
	}

	exec.Command("systemctl", "--user", "daemon-reload").Run()
	cmd := exec.Command("systemctl", "--user", "enable", "--now", "anycode-daemon.service")
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to enable systemd service. Ensure systemd user instance is running: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("✅ Successfully installed and started AnyCode Daemon as a systemd user service.")
}

func uninstallLinux() {
	homeDir, _ := os.UserHomeDir()
	servicePath := filepath.Join(homeDir, ".config", "systemd", "user", "anycode-daemon.service")

	exec.Command("systemctl", "--user", "disable", "--now", "anycode-daemon.service").Run()
	if err := os.Remove(servicePath); err != nil && !os.IsNotExist(err) {
		fmt.Printf("Failed to remove service file: %v\n", err)
	}
	exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Println("✅ Successfully uninstalled AnyCode Daemon systemd user service.")
}
