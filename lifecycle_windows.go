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
