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

const Version = "0.4.0"

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
		case "version", "--version", "-version", "-v":
			fmt.Println(Version)
			return
		}
	}
	// Backward compatible: `anycode [-port=...] [-root=...]` behaves like start.
	cmdStart(os.Args[1:])
}

func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	port := fs.Int("port", 9527, "WebSocket server port")
	root := fs.String("root", "", "Project root directory (default: cwd)")
	tokenFlag := fs.String("token", "", "Auth token (auto-generated if empty)")
	noRelay := fs.Bool("no-relay", false, "Do not connect to the cloud relay even if registered")
	showVersion := fs.Bool("version", false, "Print version and exit")
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	projectRoot := *root
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}
	projectRoot, _ = filepath.Abs(projectRoot)

	cfg := LoadConfig()
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

`, Version, projectRoot, *port, token, relayLine, bestIP, *port, *port)

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
