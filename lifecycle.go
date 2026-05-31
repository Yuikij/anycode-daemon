package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Process lifecycle management for the daemon. `anycode start -d` launches a
// detached background process; `stop` / `restart` / `status` / `log` manage it
// via a PID file and a log file under ~/.anycode/. This keeps a long-running
// daemon alive without occupying a terminal, with no root/systemd required
// (though a systemd unit can still wrap `anycode start` if desired).

func pidFilePath() string { return filepath.Join(configDir(), "daemon.pid") }
func logFilePath() string { return filepath.Join(configDir(), "daemon.log") }

func readPidFile() (int, error) {
	data, err := os.ReadFile(pidFilePath())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return pid, nil
}

func writePidFile(pid int) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(pidFilePath(), []byte(strconv.Itoa(pid)), 0o644)
}

func removePidFile() { _ = os.Remove(pidFilePath()) }

// runningPid returns the PID of a live daemon recorded in the PID file, or 0
// if none is running (also clearing a stale PID file).
func runningPid() int {
	pid, err := readPidFile()
	if err != nil || pid <= 0 {
		return 0
	}
	if processAlive(pid) {
		return pid
	}
	removePidFile()
	return 0
}

// daemonize re-executes this binary as a detached background `anycode start`
// with stdout/stderr redirected to the log file, records its PID, and returns
// the child PID. startArgs are the flags originally passed to `start`.
func daemonize(startArgs []string) (int, error) {
	if pid := runningPid(); pid != 0 {
		return 0, fmt.Errorf("daemon already running (pid %d) — use `anycode restart`", pid)
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return 0, err
	}
	logF, err := os.OpenFile(logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("open log file: %w", err)
	}
	defer logF.Close()

	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}

	args := append([]string{"start"}, startArgs...)
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "ANYCODE_DAEMONIZED=1")
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.Stdin = nil
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if err := writePidFile(pid); err != nil {
		return pid, err
	}
	// Detach so the child keeps running after the parent exits.
	_ = cmd.Process.Release()
	return pid, nil
}

// stopRunning terminates a live daemon and waits up to ~5s for it to exit.
func stopRunning() (int, bool) {
	pid := runningPid()
	if pid == 0 {
		return 0, false
	}
	_ = terminate(pid)
	for i := 0; i < 50; i++ {
		if !processAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	removePidFile()
	return pid, true
}

func cmdStop() {
	pid, was := stopRunning()
	if !was {
		fmt.Println("Daemon is not running.")
		return
	}
	fmt.Printf("Daemon stopped (pid %d).\n", pid)
}

func cmdRestart(args []string) {
	if pid, was := stopRunning(); was {
		fmt.Printf("Stopped previous daemon (pid %d).\n", pid)
	}
	pid, err := daemonize(args)
	if err != nil {
		fmt.Printf("Failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Daemon started in background (pid %d).\n", pid)
	fmt.Printf("Logs: anycode log -f   (%s)\n", logFilePath())
}

func cmdStatus() {
	pid := runningPid()
	if pid == 0 {
		fmt.Println("● AnyCode daemon: stopped")
		fmt.Printf("  Start with: anycode start -d\n")
		return
	}
	cfg := LoadConfig()
	fmt.Printf("● AnyCode daemon: running (pid %d)\n", pid)
	fmt.Printf("  Version: %s\n", Version)
	if cfg.DeviceToken != "" && cfg.RelayURL != "" {
		fmt.Printf("  Relay:   %s  device=%s\n", cfg.RelayURL, displayName(cfg))
	} else {
		fmt.Printf("  Relay:   (not registered — run `anycode register`)\n")
	}
	fmt.Printf("  Log:     %s\n", logFilePath())
}

// cmdLog prints the daemon log file. With -f it follows new output (tail -f).
func cmdLog(args []string) {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	follow := fs.Bool("f", false, "Follow log output (like tail -f)")
	lines := fs.Int("n", 200, "Number of lines to print from the end of the log")
	_ = fs.Parse(args)

	path := logFilePath()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No log yet at %s (start the daemon with `anycode start -d`).\n", path)
			return
		}
		fmt.Printf("Cannot open log: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	printLastLines(f, *lines)

	if !*follow {
		return
	}
	// Follow: seek to end and poll for appended data.
	offset, _ := f.Seek(0, io.SeekEnd)
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
			offset += int64(len(line))
		}
		if err == io.EOF {
			time.Sleep(400 * time.Millisecond)
			// Detect truncation/rotation: if file shrank, reopen.
			if fi, statErr := os.Stat(path); statErr == nil && fi.Size() < offset {
				_, _ = f.Seek(0, io.SeekStart)
				offset = 0
				reader.Reset(f)
			}
			continue
		}
		if err != nil {
			return
		}
	}
}

// printLastLines prints the final n lines of the file at f's current position.
func printLastLines(f *os.File, n int) {
	if n <= 0 {
		return
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ring := make([]string, 0, n)
	for scanner.Scan() {
		if len(ring) == n {
			ring = ring[1:]
		}
		ring = append(ring, scanner.Text())
	}
	for _, l := range ring {
		fmt.Println(l)
	}
}
