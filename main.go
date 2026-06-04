package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const Version = "0.6.7"

func main() {
	// Subcommand dispatch. `anycode start` (or no args) runs the daemon;
	// login/register/logout manage the SaaS account + device binding.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "login":
			cmdLogin(os.Args[2:])
			return
		case "register":
			cmdRegister(os.Args[2:])
			return
		case "logout":
			cmdLogout()
			return
		case "start":
			cmdStart(os.Args[2:])
			return
		case "stop":
			cmdStop()
			return
		case "restart":
			cmdRestart(os.Args[2:])
			return
		case "status":
			cmdStatus()
			return
		case "log", "logs":
			cmdLog(os.Args[2:])
			return
		case "update":
			cmdUpdate()
			return
		case "proxy":
			cmdProxy(os.Args[2:])
			return
		case "version", "--version", "-version", "-v":
			fmt.Println(Version)
			return
		case "help", "-h", "--help":
			printGlobalUsage()
			return
		}
	}
	// Backward compatible: `anycode [-port=...] [-root=...]` behaves like start.
	cmdStart(os.Args[1:])
}

func printGlobalUsage() {
	fmt.Printf(`AnyCode Daemon v%s

Usage: anycode <command> [options]

Commands:
  login      Authenticate your account and device via Web or Email
  register   Bind this machine to your AnyCode account
  start      Run the daemon (foreground or -d for background)
  status     Show daemon status (PID, Logs, Relay status)
  stop       Stop the background daemon
  restart    Restart the background daemon
  log, logs  Tail the daemon logs
  update     Self-update to the latest version
  proxy      Get, set, or clear the HTTP/HTTPS proxy
  version    Print version information

Run 'anycode <command> -h' for more details on a specific command.
`, Version)
}

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	port := fs.Int("port", 9527, "WebSocket server port")
	root := fs.String("root", "", "Project root directory (default: cwd)")
	tokenFlag := fs.String("token", "", "Auth token (auto-generated if empty)")
	noRelay := fs.Bool("no-relay", false, "Do not connect to the cloud relay even if registered")
	showVersion := fs.Bool("version", false, "Print version and exit")
	background := fs.Bool("d", false, "Run as a detached background daemon")
	backgroundLong := fs.Bool("daemon", false, "Run as a detached background daemon")
	proxy := fs.String("proxy", "", "HTTP/HTTPS Proxy URL")
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	// Background mode: re-exec ourselves detached, write a PID file, and exit.
	// The child sets ANYCODE_DAEMONIZED=1 so it skips this branch and runs
	// the server loop in the foreground (with output going to the log file).
	daemonized := os.Getenv("ANYCODE_DAEMONIZED") == "1"
	if (*background || *backgroundLong) && !daemonized {
		pid, err := daemonize(args)
		if err != nil {
			fmt.Printf("Failed to start daemon in background: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("AnyCode daemon started in background (pid %d).\n", pid)
		fmt.Printf("Status: anycode status\n")
		fmt.Printf("Logs:   anycode log -f   (%s)\n", logFilePath())
		fmt.Printf("Stop:   anycode stop\n")
		return
	}

	projectRoot := *root
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}
	projectRoot, _ = filepath.Abs(projectRoot)

	cfg := LoadConfig()

	if *proxy != "" {
		cfg.Proxy = *proxy
		cfg.Save()
	}

	if cfg.Proxy != "" {
		os.Setenv("HTTP_PROXY", cfg.Proxy)
		os.Setenv("HTTPS_PROXY", cfg.Proxy)
		os.Setenv("ALL_PROXY", cfg.Proxy)
		os.Setenv("http_proxy", cfg.Proxy)
		os.Setenv("https_proxy", cfg.Proxy)
		os.Setenv("all_proxy", cfg.Proxy)
	}

	relayEnabled := !*noRelay && cfg.DeviceToken != "" && cfg.RelayURL != ""

	// Token precedence: explicit flag > registered device token > random.
	token := *tokenFlag
	if token == "" {
		token = cfg.DeviceToken
	}
	if token == "" {
		b := make([]byte, 16)
		rand.Read(b)
		token = hex.EncodeToString(b)
	}

	bestIP := pickBestIP()

	relayLine := "  Relay:    (not registered — run `anycode register`)\n"
	if relayEnabled {
		relayLine = fmt.Sprintf("  Relay:    %s  device=%s\n", cfg.RelayURL, displayName(cfg))
	}

	fmt.Printf(`
  ╔══════════════════════════════════════╗
  ║         AnyCode Daemon v%-10s    ║
  ║             (Go Edition)            ║
  ╚══════════════════════════════════════╝

  Project:  %s
  Port:     %d
  Token:    %s

%s
  Connect from phone: ws://%s:%d
  Connect locally:    ws://localhost:%d

`, Version, projectRoot, *port, maskSecret(token), relayLine, bestIP, *port, *port)

	// When running as the detached background child, own the PID file so
	// `anycode status/stop/restart` can find us, and clean it up on exit.
	if daemonized {
		_ = writePidFile(os.Getpid())
		defer removePidFile()
	}

	if cfg.Proxy != "" {
		log.Printf("[startup] daemon v%s; upstream proxy=%s (loopback/LAN targets bypass it)", Version, cfg.Proxy)
	} else {
		log.Printf("[startup] daemon v%s; no upstream proxy configured", Version)
	}

	server := NewServer(*port, projectRoot, token)

	if relayEnabled {
		server.StartRelay(cfg.RelayURL, cfg.DeviceToken)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n  Shutting down...")
		server.codex.Stop()
		server.gemini.Stop()
		server.claude.Stop()
		if daemonized {
			removePidFile()
		}
		os.Exit(0)
	}()

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

func displayName(cfg *Config) string {
	if cfg.DeviceName != "" {
		return cfg.DeviceName
	}
	if cfg.DeviceID != "" {
		return cfg.DeviceID
	}
	return "this machine"
}

func maskSecret(value string) string {
	if len(value) <= 8 {
		return "********"
	}
	return value[:4] + "..." + value[len(value)-4:]
}

func pickBestIP() string {
	preferred := []string{"en0", "en1", "Wi-Fi", "Ethernet", "eth0", "wlan0"}

	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}

	type lanAddr struct {
		iface string
		ip    string
	}
	var addrs []lanAddr

	for _, iface := range ifaces {
		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range ifAddrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			addrs = append(addrs, lanAddr{iface: iface.Name, ip: ip.String()})
		}
	}

	if len(addrs) == 0 {
		return "127.0.0.1"
	}

	for _, pref := range preferred {
		for _, a := range addrs {
			if a.iface == pref {
				return a.ip
			}
		}
	}

	for _, a := range addrs {
		if len(a.ip) > 7 && (a.ip[:8] == "192.168." || a.ip[:3] == "10.") {
			return a.ip
		}
	}

	return addrs[0].ip
}
