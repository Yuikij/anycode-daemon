package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	out, err := gitCmd(cwd, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return nil, err
	}
	return parseGitStatusPorcelainV2Z(out), nil
}

func parseGitStatusPorcelainV2Z(output string) *GitStatus {
	status := &GitStatus{Branch: "HEAD", Files: []GitFileStatus{}}
	files := make([]GitFileStatus, 0)
	records := strings.Split(output, "\x00")
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		if strings.HasPrefix(record, "# branch.head ") {
			branch := strings.TrimSpace(strings.TrimPrefix(record, "# branch.head "))
			if branch != "" && branch != "(detached)" {
				status.Branch = branch
			}
			continue
		}
		if strings.HasPrefix(record, "# branch.ab ") {
			parts := strings.Fields(strings.TrimPrefix(record, "# branch.ab "))
			for _, p := range parts {
				if strings.HasPrefix(p, "+") {
					status.Ahead, _ = strconv.Atoi(p[1:])
				} else if strings.HasPrefix(p, "-") {
					status.Behind, _ = strconv.Atoi(p[1:])
				}
			}
			continue
		}

		switch record[0] {
		case '?':
			path := strings.TrimSpace(strings.TrimPrefix(record, "? "))
			if path != "" {
				files = append(files, GitFileStatus{Path: path, Status: "untracked", Staged: false})
			}
		case '1':
			parts := strings.SplitN(record, " ", 9)
			if len(parts) >= 9 {
				files = appendGitXYStatus(files, parts[1], parts[8])
			}
		case '2':
			parts := strings.SplitN(record, " ", 10)
			if len(parts) >= 10 {
				files = appendGitXYStatus(files, parts[1], parts[9])
			}
			if i+1 < len(records) {
				i++
			}
		case 'u':
			parts := strings.SplitN(record, " ", 11)
			if len(parts) >= 11 {
				files = append(files, GitFileStatus{Path: parts[10], Status: "conflicted", Staged: false})
			}
		}
	}
	status.Files = files
	return status
}

func appendGitXYStatus(files []GitFileStatus, xy, path string) []GitFileStatus {
	if len(xy) < 2 || path == "" {
		return files
	}
	x := xy[0]
	y := xy[1]
	if x != '.' && x != ' ' {
		files = append(files, GitFileStatus{Path: path, Status: gitStatusName(x), Staged: true})
	}
	if y != '.' && y != ' ' {
		files = append(files, GitFileStatus{Path: path, Status: gitStatusName(y), Staged: false})
	}
	return files
}

func gitStatusName(code byte) string {
	switch code {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'U':
		return "conflicted"
	default:
		return "modified"
	}
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+),?(\d*) \+(\d+),?(\d*) @@`)

func parseDiff(diffText string) []GitDiff {
	diffs := make([]GitDiff, 0)
	var currentDiff *GitDiff
	var currentHunk *DiffHunk

	flushHunk := func() {
		if currentDiff != nil && currentHunk != nil {
			currentDiff.Hunks = append(currentDiff.Hunks, *currentHunk)
			currentHunk = nil
		}
	}
	flushDiff := func() {
		flushHunk()
		if currentDiff != nil {
			diffs = append(diffs, *currentDiff)
			currentDiff = nil
		}
	}

	for _, line := range strings.Split(diffText, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flushDiff()
			currentDiff = &GitDiff{Path: parseDiffGitHeaderPath(line)}
			continue
		}
		if currentDiff == nil {
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			if path := parseDiffFileHeaderPath(line, "+++ ", "b/"); path != "" {
				currentDiff.Path = path
			}
		}

		m := hunkRe.FindStringSubmatch(line)
		if m != nil {
			flushHunk()
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
			currentHunk = &DiffHunk{
				OldStart: oldStart, OldLines: oldLines,
				NewStart: newStart, NewLines: newLines,
				Content: line + "\n",
			}
			continue
		}

		if currentHunk != nil && isDiffHunkLine(line) {
			currentHunk.Content += line + "\n"
		}
	}

	flushDiff()
	return diffs
}

func isDiffHunkLine(line string) bool {
	return strings.HasPrefix(line, "+") ||
		strings.HasPrefix(line, "-") ||
		strings.HasPrefix(line, " ") ||
		strings.HasPrefix(line, `\`)
}

func parseDiffGitHeaderPath(line string) string {
	fields := splitGitHeaderFields(strings.TrimPrefix(line, "diff --git "))
	if len(fields) >= 2 {
		return stripGitDiffPathPrefix(fields[1], "b/")
	}
	if len(fields) == 1 {
		return stripGitDiffPathPrefix(fields[0], "a/")
	}
	return ""
}

func parseDiffFileHeaderPath(line, prefix, pathPrefix string) string {
	path := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = path[:tab]
	}
	path = decodeGitQuotedPath(path)
	if path == "/dev/null" {
		return ""
	}
	return stripGitDiffPathPrefix(path, pathPrefix)
}

func splitGitHeaderFields(value string) []string {
	var fields []string
	for len(value) > 0 {
		value = strings.TrimLeft(value, " ")
		if value == "" {
			break
		}
		if value[0] == '"' {
			end := 1
			escaped := false
			for end < len(value) {
				ch := value[end]
				if escaped {
					escaped = false
				} else if ch == '\\' {
					escaped = true
				} else if ch == '"' {
					end++
					break
				}
				end++
			}
			fields = append(fields, decodeGitQuotedPath(value[:end]))
			value = value[end:]
			continue
		}
		next := strings.IndexByte(value, ' ')
		if next < 0 {
			fields = append(fields, value)
			break
		}
		fields = append(fields, value[:next])
		value = value[next+1:]
	}
	return fields
}

func decodeGitQuotedPath(path string) string {
	if strings.HasPrefix(path, `"`) && strings.HasSuffix(path, `"`) {
		if decoded, err := strconv.Unquote(path); err == nil {
			return decoded
		}
	}
	return path
}

func stripGitDiffPathPrefix(path, prefix string) string {
	path = decodeGitQuotedPath(strings.TrimSpace(path))
	if strings.HasPrefix(path, prefix) {
		return strings.TrimPrefix(path, prefix)
	}
	return path
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
		"--format=%H%x1f%h%x1f%s%x1f%an%x1f%aI%x1e")
	if err != nil {
		return nil, err
	}
	return parseGitLog(out), nil
}

func parseGitLog(output string) []GitLogEntry {
	entries := make([]GitLogEntry, 0)
	blocks := strings.Split(output, "\x1e")
	for _, block := range blocks {
		fields := strings.Split(block, "\x1f")
		if len(fields) < 5 {
			continue
		}
		entries = append(entries, GitLogEntry{
			Hash:      strings.TrimSpace(fields[0]),
			HashShort: strings.TrimSpace(fields[1]),
			Message:   fields[2],
			Author:    fields[3],
			Date:      strings.TrimSpace(fields[4]),
		})
	}
	return entries
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
	untracked, _ := gitCmd(gitRoot, "ls-files", "--others", "--exclude-standard", "-z")
	files := splitNULList(untracked)
	sort.Strings(files)
	return map[string]interface{}{
		"diff":           diff,
		"untrackedFiles": files,
		"gitRoot":        gitRoot,
	}, nil
}

func splitNULList(output string) []string {
	items := make([]string, 0)
	for _, item := range strings.Split(output, "\x00") {
		if item != "" {
			items = append(items, item)
		}
	}
	return items
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
