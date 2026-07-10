//go:build windows

package main

import (
	"os"
	"syscall"
)

const (
	_DETACHED_PROCESS         = 0x00000008
	_CREATE_NEW_PROCESS_GROUP = 0x00000200
	_STILL_ACTIVE             = 259
	_PROCESS_QUERY_LIMITED    = 0x1000
)

// detachSysProcAttr starts the child detached from the parent console.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: _DETACHED_PROCESS | _CREATE_NEW_PROCESS_GROUP}
}

// agentSysProcAttr gives a spawned agent CLI its own process group. Windows has
// no POSIX process groups to signal, but a separate group at least isolates
// console ctrl events from the daemon.
func agentSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: _CREATE_NEW_PROCESS_GROUP}
}

// killAgentProcess kills the agent process. Windows cannot signal a whole
// process group from here, so this only kills the direct child.
func killAgentProcess(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	return proc.Kill()
}

// processAlive reports whether a process with the given PID is still running.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(_PROCESS_QUERY_LIMITED, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == _STILL_ACTIVE
}

// terminate stops the daemon. Windows has no SIGTERM, so we kill the process.
func terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}
