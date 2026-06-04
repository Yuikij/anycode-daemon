package main

import "testing"

func TestParseGitStatusPorcelainV2Z(t *testing.T) {
	output := stringsJoinNUL([]string{
		"# branch.head feature/test",
		"# branch.ab +2 -1",
		"1 MM N... 100644 100644 100644 abc abc src/app.go",
		"2 R. N... 100644 100644 100644 abc abc R100 new name.txt",
		"old name.txt",
		"? untracked file.txt",
	})

	status := parseGitStatusPorcelainV2Z(output)
	if status.Branch != "feature/test" || status.Ahead != 2 || status.Behind != 1 {
		t.Fatalf("unexpected branch state: %#v", status)
	}
	if len(status.Files) != 4 {
		t.Fatalf("expected 4 file status entries, got %#v", status.Files)
	}
	want := []GitFileStatus{
		{Path: "src/app.go", Status: "modified", Staged: true},
		{Path: "src/app.go", Status: "modified", Staged: false},
		{Path: "new name.txt", Status: "renamed", Staged: true},
		{Path: "untracked file.txt", Status: "untracked", Staged: false},
	}
	for i := range want {
		if status.Files[i] != want[i] {
			t.Fatalf("entry %d: want %#v, got %#v", i, want[i], status.Files[i])
		}
	}
}

func TestParseDiffHandlesQuotedPathsAndNoNewlineMarker(t *testing.T) {
	diff := "diff --git \"a/space name.txt\" \"b/space name.txt\"\n" +
		"index 111..222 100644\n" +
		"--- \"a/space name.txt\"\n" +
		"+++ \"b/space name.txt\"\n" +
		"@@ -1 +1 @@\n" +
		"-old\n" +
		"+new\n" +
		`\\ No newline at end of file` + "\n"

	parsed := parseDiff(diff)
	if len(parsed) != 1 {
		t.Fatalf("expected 1 diff, got %#v", parsed)
	}
	if parsed[0].Path != "space name.txt" {
		t.Fatalf("unexpected path: %q", parsed[0].Path)
	}
	if len(parsed[0].Hunks) != 1 {
		t.Fatalf("expected 1 hunk, got %#v", parsed[0].Hunks)
	}
	if parsed[0].Hunks[0].OldStart != 1 || parsed[0].Hunks[0].NewStart != 1 {
		t.Fatalf("unexpected hunk coordinates: %#v", parsed[0].Hunks[0])
	}
	if parsed[0].Hunks[0].Content == "" {
		t.Fatal("hunk content should be retained")
	}
}

func TestParseGitLogUsesRecordSeparators(t *testing.T) {
	output := "aaaaaaaa\x1faaaaaaa\x1fsubject with --- marker\x1fAlice\x1f2026-06-04T00:00:00Z\x1e" +
		"bbbbbbbb\x1fbbbbbbb\x1fsecond\x1fBob\x1f2026-06-05T00:00:00Z\x1e"

	entries := parseGitLog(output)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %#v", entries)
	}
	if entries[0].Message != "subject with --- marker" || entries[1].Author != "Bob" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func stringsJoinNUL(parts []string) string {
	out := ""
	for _, part := range parts {
		out += part + "\x00"
	}
	return out
}
