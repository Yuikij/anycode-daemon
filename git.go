package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type GitFileStatus struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Staged bool   `json:"staged"`
}

type GitStatus struct {
	Branch string          `json:"branch"`
	Ahead  int             `json:"ahead"`
	Behind int             `json:"behind"`
	Files  []GitFileStatus `json:"files"`
}

type DiffHunk struct {
	OldStart int    `json:"oldStart"`
	OldLines int    `json:"oldLines"`
	NewStart int    `json:"newStart"`
	NewLines int    `json:"newLines"`
	Content  string `json:"content"`
}

type GitDiff struct {
	Path  string     `json:"path"`
	Hunks []DiffHunk `json:"hunks"`
}

type GitLogEntry struct {
	Hash      string `json:"hash"`
	HashShort string `json:"hashShort"`
	Message   string `json:"message"`
	Author    string `json:"author"`
	Date      string `json:"date"`
}

func gitCmd(cwd string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(out) > 0 {
			_ = ee
			return string(out), nil
		}
		return "", err
	}
	return string(out), nil
}

func getGitStatus(cwd string) (*GitStatus, error) {
	branchOut, err := gitCmd(cwd, "branch", "--show-current")
	if err != nil {
		return nil, err
	}
	branch := strings.TrimSpace(branchOut)
	if branch == "" {
		branch = "HEAD"
	}

	var ahead, behind int
	statusSB, _ := gitCmd(cwd, "status", "--porcelain=v2", "--branch")
	for _, line := range strings.Split(statusSB, "\n") {
		if strings.HasPrefix(line, "# branch.ab") {
			parts := strings.Fields(line)
			for _, p := range parts {
				if strings.HasPrefix(p, "+") {
					ahead, _ = strconv.Atoi(p[1:])
				} else if strings.HasPrefix(p, "-") {
					behind, _ = strconv.Atoi(p[1:])
				}
			}
		}
	}

	porcelain, err := gitCmd(cwd, "status", "--porcelain")
	if err != nil {
		return nil, err
	}

	files := make([]GitFileStatus, 0)
	for _, line := range strings.Split(strings.TrimSpace(porcelain), "\n") {
		if len(line) < 4 {
			continue
		}
		x := line[0]
		y := line[1]
		path := strings.TrimSpace(line[3:])

		if strings.Contains(path, " -> ") {
			parts := strings.SplitN(path, " -> ", 2)
			path = parts[1]
		}

		status := "modified"
		staged := false

		switch {
		case x == '?' && y == '?':
			status = "untracked"
		case x == 'A':
			status = "added"
			staged = true
		case x == 'D' || y == 'D':
			status = "deleted"
			staged = x == 'D'
		case x == 'R':
			status = "renamed"
			staged = true
		case x == 'M':
			staged = true
		}

		if y == 'M' && x != ' ' {
			files = append(files, GitFileStatus{Path: path, Status: status, Staged: true})
			files = append(files, GitFileStatus{Path: path, Status: "modified", Staged: false})
			continue
		}

		files = append(files, GitFileStatus{Path: path, Status: status, Staged: staged})
	}

	return &GitStatus{Branch: branch, Ahead: ahead, Behind: behind, Files: files}, nil
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+),?(\d*) \+(\d+),?(\d*) @@`)

func parseDiff(diffText string) []GitDiff {
	diffs := make([]GitDiff, 0)
	chunks := strings.Split(diffText, "diff --git ")
	for _, chunk := range chunks {
		if chunk == "" {
			continue
		}
		lines := strings.Split(chunk, "\n")
		headerParts := strings.SplitN(lines[0], " b/", 2)
		if len(headerParts) < 2 {
			continue
		}
		filePath := headerParts[1]

		hunks := make([]DiffHunk, 0)
		var current *DiffHunk
		for _, line := range lines {
			m := hunkRe.FindStringSubmatch(line)
			if m != nil {
				if current != nil {
					hunks = append(hunks, *current)
				}
				oldStart, _ := strconv.Atoi(m[1])
				oldLines := 1
				if m[2] != "" {
					oldLines, _ = strconv.Atoi(m[2])
				}
				newStart, _ := strconv.Atoi(m[3])
				newLines := 1
				if m[4] != "" {
					newLines, _ = strconv.Atoi(m[4])
				}
				current = &DiffHunk{
					OldStart: oldStart, OldLines: oldLines,
					NewStart: newStart, NewLines: newLines,
					Content: line + "\n",
				}
			} else if current != nil && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ")) {
				current.Content += line + "\n"
			}
		}
		if current != nil {
			hunks = append(hunks, *current)
		}
		diffs = append(diffs, GitDiff{Path: filePath, Hunks: hunks})
	}
	return diffs
}

func getGitDiff(cwd string, filePath string) ([]GitDiff, error) {
	args := []string{"diff"}
	if filePath != "" {
		args = append(args, "--", filePath)
	}
	out, err := gitCmd(cwd, args...)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

func getGitDiffStaged(cwd string, filePath string) ([]GitDiff, error) {
	args := []string{"diff", "--cached"}
	if filePath != "" {
		args = append(args, "--", filePath)
	}
	out, err := gitCmd(cwd, args...)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

func getGitLog(cwd string, count int) ([]GitLogEntry, error) {
	out, err := gitCmd(cwd, "log", fmt.Sprintf("--max-count=%d", count),
		"--format=%H%n%h%n%s%n%an%n%aI%n---")
	if err != nil {
		return nil, err
	}

	entries := make([]GitLogEntry, 0)
	blocks := strings.Split(strings.TrimSpace(out), "---\n")
	for _, block := range blocks {
		lines := strings.SplitN(strings.TrimSpace(block), "\n", 5)
		if len(lines) < 5 {
			continue
		}
		entries = append(entries, GitLogEntry{
			Hash:      lines[0],
			HashShort: lines[1],
			Message:   lines[2],
			Author:    lines[3],
			Date:      lines[4],
		})
	}
	return entries, nil
}

func getGitFileDiff(cwd, commitHash, filePath string) ([]GitDiff, error) {
	args := []string{"diff", commitHash + "^", commitHash}
	if filePath != "" {
		args = append(args, "--", filePath)
	}
	out, err := gitCmd(cwd, args...)
	if err != nil {
		return nil, err
	}
	return parseDiff(out), nil
}

func getGitDiffHead(cwd string) (map[string]interface{}, error) {
	gitRoot := findGitRoot(cwd)
	diff, err := gitCmd(gitRoot, "diff", "HEAD")
	if err != nil {
		return nil, err
	}
	untracked, _ := gitCmd(gitRoot, "ls-files", "--others", "--exclude-standard")
	files := make([]string, 0)
	for _, f := range strings.Split(strings.TrimSpace(untracked), "\n") {
		if f != "" {
			files = append(files, f)
		}
	}
	return map[string]interface{}{
		"diff":           diff,
		"untrackedFiles": files,
		"gitRoot":        gitRoot,
	}, nil
}

func findGitRoot(startDir string) string {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return startDir
}

func gitApplyReverse(filePath, unifiedDiff string) error {
	cwd := findGitRoot(filepath.Dir(filePath))
	gitAddPaths(cwd, []string{filePath})

	cmd := exec.Command("git", "apply", "-R", "-")
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(unifiedDiff)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func gitApplyForward(filePath, unifiedDiff string) error {
	cwd := findGitRoot(filepath.Dir(filePath))
	gitAddPaths(cwd, []string{filePath})

	cmd := exec.Command("git", "apply", "-")
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(unifiedDiff)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func gitAddPaths(cwd string, paths []string) {
	var existing []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return
	}
	args := append([]string{"add", "--"}, existing...)
	cmd := exec.Command("git", args...)
	cmd.Dir = cwd
	_ = cmd.Run()
}

func buildReversibleDiff(filePath, rawDiff string) string {
	if strings.HasPrefix(rawDiff, "diff --git") {
		return rawDiff
	}
	base := filepath.Base(filePath)
	if strings.Contains(rawDiff, "@@") {
		return fmt.Sprintf("diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n%s", base, base, base, base, rawDiff)
	}
	lines := strings.Split(rawDiff, "\n")
	var nonEmpty []string
	for _, l := range lines {
		if l != "" {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) == 0 {
		return rawDiff
	}
	var plusLines []string
	for _, l := range nonEmpty {
		plusLines = append(plusLines, "+"+l)
	}
	return fmt.Sprintf("diff --git a/%s b/%s\nnew file mode 100644\n--- /dev/null\n+++ b/%s\n@@ -0,0 +1,%d @@\n%s\n",
		base, base, base, len(nonEmpty), strings.Join(plusLines, "\n"))
}
