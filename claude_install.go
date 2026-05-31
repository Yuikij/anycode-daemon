package main

import (
	"log"
	"os/exec"
	"sync"
	"time"
)

// claudeAcpPackage is the npm package that provides the `claude-code-acp`
// binary (the Zed-maintained ACP bridge for Claude Code).
const claudeAcpPackage = "@zed-industries/claude-code-acp"

var (
	claudeInstallMu    sync.Mutex
	claudeInstallTried bool
)

// ensureClaudeAcp makes a best-effort attempt to make `claude-code-acp`
// runnable. If the supplied check already passes, it returns true. Otherwise,
// when npm is on PATH, it runs `npm install -g @zed-industries/claude-code-acp`
// exactly once per daemon run and rechecks.
//
// This lets every install path (curl installer, npm wrapper, manual binary)
// self-heal the Claude dependency the first time a user actually starts Claude,
// instead of failing with an "install it yourself" error.
func ensureClaudeAcp(check func() bool) bool {
	if check() {
		return true
	}

	claudeInstallMu.Lock()
	defer claudeInstallMu.Unlock()

	// Re-check under the lock in case a concurrent caller just installed it.
	if check() {
		return true
	}
	if claudeInstallTried {
		return false
	}
	claudeInstallTried = true

	npm, err := exec.LookPath("npm")
	if err != nil {
		log.Printf("[claude] %s not found and npm is unavailable; install Node.js, then run: npm install -g %s",
			claudeAcpCommand(), claudeAcpPackage)
		return false
	}

	log.Printf("[claude] %s not found — installing %s via npm (one-time, this may take a minute)...",
		claudeAcpCommand(), claudeAcpPackage)
	cmd := exec.Command(npm, "install", "-g", claudeAcpPackage)
	out, runErr := cmd.CombinedOutput()
	if runErr != nil {
		log.Printf("[claude] auto-install failed: %v\n%s", runErr, tailString(string(out), 800))
		log.Printf("[claude] you can install it manually: npm install -g %s", claudeAcpPackage)
		return false
	}
	log.Printf("[claude] %s installed", claudeAcpPackage)

	// Give the shell command hash / PATH a moment to settle, then recheck.
	time.Sleep(200 * time.Millisecond)
	return check()
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
