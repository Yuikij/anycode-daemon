package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func commonAgentBinDirs() []string {
	dirs := []string{"/usr/local/bin"}
	if runtime.GOOS == "darwin" {
		dirs = append(dirs, "/opt/homebrew/bin")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append([]string{
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
		}, dirs...)
	}
	return dirs
}

func augmentAgentPath() {
	current := os.Getenv("PATH")
	parts := filepath.SplitList(current)
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		if part != "" {
			seen[part] = true
		}
	}

	prefix := []string{}
	for _, dir := range commonAgentBinDirs() {
		if seen[dir] || !dirExists(dir) {
			continue
		}
		prefix = append(prefix, dir)
		seen[dir] = true
	}
	if len(prefix) == 0 {
		return
	}
	next := append(prefix, parts...)
	os.Setenv("PATH", strings.Join(next, string(os.PathListSeparator)))
}

func resolveCommandInCommonAgentBins(names ...string) string {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, dir := range commonAgentBinDirs() {
			path := filepath.Join(dir, name)
			if isExecutableFile(path) {
				return path
			}
		}
	}
	return ""
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
