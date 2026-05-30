package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type FileEntry struct {
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	Type     string       `json:"type"`
	Size     int64        `json:"size"`
	Modified string       `json:"modified"`
	Children *[]FileEntry `json:"children,omitempty"`
}

type BrowseResult struct {
	Path   string      `json:"path"`
	Parent string      `json:"parent"`
	Items  []FileEntry `json:"items"`
}

type FileContent struct {
	Path     string `json:"path"`
	Name     string `json:"name,omitempty"`
	Content  string `json:"content"`
	Language string `json:"language"`
	Size     int64  `json:"size"`
	Lines    int    `json:"lines"`
	Encoding string `json:"encoding,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type ProjectInfo struct {
	Name      string `json:"name"`
	Root      string `json:"root"`
	IsGit     bool   `json:"isGit"`
	FileCount int    `json:"fileCount"`
}

var languageMap = map[string]string{
	".ts": "typescript", ".tsx": "tsx", ".js": "javascript", ".jsx": "jsx",
	".py": "python", ".rs": "rust", ".go": "go", ".rb": "ruby",
	".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".swift": "swift",
	".c": "c", ".h": "c", ".cpp": "cpp", ".cc": "cpp", ".hpp": "cpp", ".cs": "csharp",
	".php": "php", ".html": "html", ".htm": "html",
	".css": "css", ".scss": "scss", ".less": "less",
	".json": "json", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml", ".xml": "xml",
	".md": "markdown", ".mdx": "mdx", ".sql": "sql",
	".sh": "bash", ".bash": "bash", ".zsh": "bash", ".fish": "fish",
	".dockerfile": "dockerfile", ".graphql": "graphql", ".gql": "graphql",
	".vue": "vue", ".svelte": "svelte", ".lua": "lua", ".r": "r",
	".dart": "dart", ".ex": "elixir", ".exs": "elixir",
	".erl": "erlang", ".hs": "haskell", ".ml": "ocaml",
	".clj": "clojure", ".scala": "scala", ".zig": "zig", ".nim": "nim",
	".proto": "protobuf", ".tf": "hcl", ".lock": "text", ".env": "dotenv",
	".gitignore": "gitignore",
}

var filenameLanguage = map[string]string{
	"Dockerfile": "dockerfile", "Makefile": "makefile",
	"Rakefile": "ruby", "Gemfile": "ruby", "Podfile": "ruby",
	".gitignore": "gitignore", ".editorconfig": "ini",
}

var imageExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".bmp": true, ".webp": true, ".ico": true, ".heic": true,
}

var imageMime = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".bmp": "image/bmp", ".webp": "image/webp",
	".ico": "image/x-icon", ".heic": "image/heic",
}

var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".bmp": true,
	".ico": true, ".webp": true, ".heic": true, ".svg": true,
	".mp3": true, ".mp4": true, ".wav": true, ".avi": true, ".mov": true, ".mkv": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".7z": true, ".rar": true,
	".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".o": true, ".a": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".pyc": true, ".class": true, ".wasm": true,
}

var ignoreDirs = map[string]bool{
	"node_modules": true, ".git": true, ".svn": true, ".hg": true,
	"__pycache__": true, ".pytest_cache": true, ".mypy_cache": true,
	"dist": true, "build": true, ".next": true, ".nuxt": true,
	".venv": true, "venv": true, ".env": true,
	"target": true, ".gradle": true,
	".idea": true, ".vscode": true,
	"Pods": true, ".build": true,
	"coverage": true, ".nyc_output": true,
}

func detectLanguage(filePath string) string {
	base := filepath.Base(filePath)
	if lang, ok := filenameLanguage[base]; ok {
		return lang
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	if lang, ok := languageMap[ext]; ok {
		return lang
	}
	return "text"
}

func browseDirectory(dirPath string, showHidden bool) (*BrowseResult, error) {
	resolved, err := filepath.Abs(dirPath)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(resolved)
	if err != nil {
		return nil, err
	}

	items := make([]FileEntry, 0)
	for _, entry := range entries {
		if !showHidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		fullPath := filepath.Join(resolved, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		entryType := "file"
		if entry.IsDir() {
			entryType = "directory"
		} else if entry.Type()&os.ModeSymlink != 0 {
			entryType = "symlink"
		}
		items = append(items, FileEntry{
			Name:     entry.Name(),
			Path:     fullPath,
			Type:     entryType,
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Type == "directory" && items[j].Type != "directory" {
			return true
		}
		if items[i].Type != "directory" && items[j].Type == "directory" {
			return false
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	return &BrowseResult{
		Path:   resolved,
		Parent: filepath.Dir(resolved),
		Items:  items,
	}, nil
}

func readAbsoluteFile(filePath string) (*FileContent, error) {
	resolved, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(resolved))

	maxSize := int64(1024 * 1024)
	if binaryExts[ext] {
		maxSize = 10 * 1024 * 1024
	}
	if info.Size() > maxSize {
		return nil, fmt.Errorf("file too large: %d bytes", info.Size())
	}

	if imageExts[ext] {
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, err
		}
		mime := imageMime[ext]
		if mime == "" {
			mime = "image/jpeg"
		}
		return &FileContent{
			Path:     resolved,
			Name:     filepath.Base(resolved),
			Content:  base64.StdEncoding.EncodeToString(data),
			Language: "image",
			Encoding: "base64",
			MimeType: mime,
			Size:     info.Size(),
			Lines:    0,
		}, nil
	}

	if ext == ".pdf" {
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, err
		}
		return &FileContent{
			Path:     resolved,
			Name:     filepath.Base(resolved),
			Content:  base64.StdEncoding.EncodeToString(data),
			Language: "pdf",
			Encoding: "base64",
			MimeType: "application/pdf",
			Size:     info.Size(),
			Lines:    0,
		}, nil
	}

	if binaryExts[ext] {
		return &FileContent{
			Path:     resolved,
			Name:     filepath.Base(resolved),
			Content:  "",
			Language: "binary",
			Size:     info.Size(),
			Lines:    0,
		}, nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	content := string(data)
	lines := strings.Count(content, "\n") + 1

	return &FileContent{
		Path:     resolved,
		Name:     filepath.Base(resolved),
		Content:  content,
		Language: detectLanguage(resolved),
		Size:     info.Size(),
		Lines:    lines,
	}, nil
}

func listDirectory(dirPath, rootPath string) ([]FileEntry, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}

	result := make([]FileEntry, 0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".gitignore" {
			continue
		}
		if ignoreDirs[entry.Name()] {
			continue
		}
		fullPath := filepath.Join(dirPath, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		relPath, _ := filepath.Rel(rootPath, fullPath)
		entryType := "file"
		if entry.IsDir() {
			entryType = "directory"
		}
		result = append(result, FileEntry{
			Name:     entry.Name(),
			Path:     relPath,
			Type:     entryType,
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z"),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Type == "directory" && result[j].Type != "directory" {
			return true
		}
		if result[i].Type != "directory" && result[j].Type == "directory" {
			return false
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})

	return result, nil
}

func getFileTree(dirPath, rootPath string, depth int) ([]FileEntry, error) {
	if depth <= 0 {
		return make([]FileEntry, 0), nil
	}
	entries, err := listDirectory(dirPath, rootPath)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].Type == "directory" && depth > 1 {
			fullPath := filepath.Join(rootPath, entries[i].Path)
			children, _ := getFileTree(fullPath, rootPath, depth-1)
			entries[i].Children = &children
		}
	}
	return entries, nil
}

func readFileContent(filePath, rootPath string) (*FileContent, error) {
	fullPath := filepath.Join(rootPath, filePath)
	abs, _ := filepath.Abs(fullPath)
	absRoot, _ := filepath.Abs(rootPath)
	if !strings.HasPrefix(abs, absRoot) {
		return nil, fmt.Errorf("path traversal not allowed")
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.Size() > 1024*1024 {
		return nil, fmt.Errorf("file too large: %d bytes (max 1048576)", info.Size())
	}
	ext := strings.ToLower(filepath.Ext(abs))
	if binaryExts[ext] {
		return nil, fmt.Errorf("binary file, cannot display as text")
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	content := string(data)
	return &FileContent{
		Path:     filePath,
		Content:  content,
		Language: detectLanguage(abs),
		Size:     info.Size(),
		Lines:    strings.Count(content, "\n") + 1,
	}, nil
}

func getProjectInfo(rootPath string) *ProjectInfo {
	name := filepath.Base(rootPath)
	_, err := os.Stat(filepath.Join(rootPath, ".git"))
	isGit := err == nil

	fileCount := 0
	var countFiles func(dir string, depth int)
	countFiles = func(dir string, depth int) {
		if depth > 5 {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if ignoreDirs[entry.Name()] {
				continue
			}
			if entry.IsDir() {
				countFiles(filepath.Join(dir, entry.Name()), depth+1)
			} else {
				fileCount++
			}
		}
	}
	countFiles(rootPath, 0)

	return &ProjectInfo{Name: name, Root: rootPath, IsGit: isGit, FileCount: fileCount}
}

func listProjectDirs() map[string]interface{} {
	home, _ := os.UserHomeDir()
	scanDirs := []string{
		filepath.Join(home, "code"), filepath.Join(home, "Code"),
		filepath.Join(home, "projects"), filepath.Join(home, "Projects"),
		filepath.Join(home, "dev"), filepath.Join(home, "Dev"),
		filepath.Join(home, "src"),
	}
	projects := make([]map[string]string, 0)
	for _, dir := range scanDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
				projects = append(projects, map[string]string{
					"name": entry.Name(),
					"path": filepath.Join(dir, entry.Name()),
				})
			}
		}
	}
	return map[string]interface{}{"projects": projects}
}
