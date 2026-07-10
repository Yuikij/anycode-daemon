//go:build !windows

package main

import (
	"os"
	"syscall"
)

// detachSysProcAttr starts the child in its own session so it survives the
// parent terminal closing (true daemonization on macOS/Linux).
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// agentSysProcAttr puts a spawned agent CLI in its own process group so
// killAgentProcess can take down the whole tree. Agent CLIs (claude-code-acp,
// cursor `agent acp`, codex app-server) routinely fork their own children;
// killing only the direct child would orphan those grandchildren, which keep
// running (and keep burning API quota) after Stop.
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// killAgentProcess kills the agent's entire process group, falling back to
// killing just the direct child if the group signal fails.
func killAgentProcess(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return proc.Kill()
}

// processAlive reports whether a process with the given PID exists.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 performs error checking without actually delivering a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}

// terminate asks the daemon to shut down gracefully.
func terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
