package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// cmdUpdate handles the self-update process for the daemon.
func cmdUpdate() {
	fmt.Printf("Checking for updates... (current version: %s)\n", Version)

	// Determine OS and architecture
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// GitHub repository for the daemon
	repo := "Yuikij/anycode-daemon"
	if customRepo := os.Getenv("ANYCODE_REPO"); customRepo != "" {
		repo = customRepo
	}

	// 1. Fetch latest release info
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		os.Exit(1)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	// Determine current executable path early to check for npm install
	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("Error finding executable path: %v\n", err)
		os.Exit(1)
	}
	if resolvedPath, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolvedPath
	}

	if strings.Contains(exePath, "node_modules") {
		fmt.Printf("\n⚠️ 检测到你可能是通过 npm 安装的 (路径包含 node_modules)。\n")
		fmt.Printf("   请勿使用 anycode update，以免破坏 npm 的包结构。\n")
		fmt.Printf("   请使用以下命令进行更新：\n\n")
		fmt.Printf("   npm update -g anycode-daemon\n\n")
		os.Exit(1)
	}

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error checking for updates: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Failed to get latest release (HTTP %d)\n", resp.StatusCode)
		os.Exit(1)
	}

	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		fmt.Printf("Error parsing release info: %v\n", err)
		os.Exit(1)
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	if latestVersion == Version {
		fmt.Println("You are already on the latest version.")
		return
	}

	fmt.Printf("Found new version: %s (downloading...)\n", release.TagName)

	// 2. Construct download URLs
	assetName := fmt.Sprintf("anycode-daemon-%s-%s", goos, goarch)
	if goos == "windows" {
		assetName += ".exe"
	}
	githubURL := fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", repo, assetName)
	cdnURL := fmt.Sprintf("https://install.anycodeapp.com/daemon/latest/%s", assetName)

	// 3. Download the new binary
	tmpPath := exePath + ".tmp"
	oldPath := exePath + ".old"

	downloadFile := func(url string) error {
		fmt.Printf("Attempting to download from: %s\n", url)
		out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("create temp file: %v\n(You might need to run this command with sudo or Administrator privileges)", err)
		}
		defer out.Close()

		dlClient := &http.Client{Timeout: 10 * time.Minute} // Allow up to 10 minutes for download
		dlReq, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return fmt.Errorf("create request: %v", err)
		}
		dlResp, err := dlClient.Do(dlReq)
		if err != nil {
			return fmt.Errorf("download: %v", err)
		}
		defer dlResp.Body.Close()

		if dlResp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", dlResp.StatusCode)
		}

		_, err = io.Copy(out, dlResp.Body)
		return err
	}

	if err := downloadFile(cdnURL); err != nil {
		fmt.Printf("→ CDN download failed: %v. Falling back to GitHub...\n", err)
		if err := downloadFile(githubURL); err != nil {
			fmt.Printf("→ GitHub download failed: %v\n", err)
			os.Remove(tmpPath)
			os.Exit(1)
		}
	}

	// 4. Replace the old binary
	// Remove any existing .old file
	_ = os.Remove(oldPath)

	// Rename current to .old
	err = os.Rename(exePath, oldPath)
	if err != nil {
		os.Remove(tmpPath)
		fmt.Printf("Error replacing current binary (rename to .old failed): %v\n(You might need to run this command with sudo)\n", err)
		os.Exit(1)
	}

	// Rename .tmp to current
	err = os.Rename(tmpPath, exePath)
	if err != nil {
		// Rollback
		os.Rename(oldPath, exePath)
		os.Remove(tmpPath)
		fmt.Printf("Error replacing current binary (rename to current failed): %v\n", err)
		os.Exit(1)
	}

	// Attempt to remove .old (might fail on Windows if process is running, which is fine)
	_ = os.Remove(oldPath)

	fmt.Printf("Successfully updated to %s!\n", release.TagName)
	fmt.Println("Please run `anycode restart` to apply the update to the background daemon.")
}
