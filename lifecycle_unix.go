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
