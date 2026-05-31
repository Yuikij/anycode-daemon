package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

// apiRequest performs a JSON HTTP request against the relay's HTTP API.
func apiRequest(relayURL, method, path string, body interface{}, session string) (map[string]interface{}, int, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, strings.TrimRight(relayURL, "/")+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.Header.Set("Authorization", "Bearer "+session)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var parsed map[string]interface{}
	_ = json.Unmarshal(data, &parsed)
	return parsed, resp.StatusCode, nil
}

func prompt(label string) string {
	fmt.Print(label)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// cmdLogin authenticates the *account* (anycode login) and stores a session
// token locally. This is the credential used to call the devices API.
func cmdLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	relay := fs.String("relay", "", "Relay/API base URL")
	email := fs.String("email", "", "Account email (bypasses device flow)")
	password := fs.String("password", "", "Account password")
	_ = fs.Parse(args)

	cfg := LoadConfig()
	if *relay != "" {
		cfg.RelayURL = *relay
	}

	if *email != "" || *password != "" {
		// Legacy email/password flow
		em := *email
		if em == "" {
			em = prompt("Email: ")
		}
		pw := *password
		if pw == "" {
			pw = prompt("Password: ")
		}

		parsed, status, err := apiRequest(cfg.RelayURL, "POST", "/api/auth/login", map[string]string{
			"email": em, "password": pw,
		}, "")
		if err != nil {
			fmt.Printf("Login failed: %v\n", err)
			os.Exit(1)
		}
		if status != 200 {
			fmt.Printf("Login failed: %v\n", apiError(parsed, status))
			os.Exit(1)
		}
		token, _ := parsed["token"].(string)
		if token == "" {
			fmt.Println("Login failed: no token returned")
			os.Exit(1)
		}
		cfg.Session = token
		cfg.UserEmail = em
		if err := cfg.Save(); err != nil {
			fmt.Printf("Could not save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Logged in as %s\n", em)
		fmt.Println("Next: run `anycode register` to register this machine, then `anycode start`.")
		return
	}

	// Device Flow
	parsed, status, err := apiRequest(cfg.RelayURL, "POST", "/api/auth/device/init", nil, "")
	if err != nil || status != 200 {
		fmt.Printf("Failed to initialize device login: %v (HTTP %d)\n", err, status)
		os.Exit(1)
	}

	deviceCode, _ := parsed["device_code"].(string)
	userCode, _ := parsed["user_code"].(string)
	verificationUri, _ := parsed["verification_uri"].(string)

	fmt.Printf("\n  请在浏览器中打开以下链接：\n")
	fmt.Printf("  %s\n\n", verificationUri)
	fmt.Printf("  输入以下设备码进行授权：\n")
	fmt.Printf("  %s\n\n", userCode)
	fmt.Printf("等待网页端确认...\n")

	for {
		time.Sleep(3 * time.Second)
		pollParsed, pollStatus, err := apiRequest(cfg.RelayURL, "POST", "/api/auth/device/poll", map[string]string{
			"device_code": deviceCode,
		}, "")
		
		if err != nil || pollStatus != 200 {
			if pollStatus == 400 && pollParsed["error"] != nil {
				fmt.Printf("\n授权失败或已过期：%v\n", pollParsed["error"])
				os.Exit(1)
			}
			continue
		}

		statusStr, _ := pollParsed["status"].(string)
		if statusStr == "verified" {
			token, _ := pollParsed["session_token"].(string)
			cfg.Session = token
			if err := cfg.Save(); err != nil {
				fmt.Printf("\nCould not save config: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("\n✅ 登录成功！\n")
			fmt.Println("Next: run `anycode register` to register this machine, then `anycode start`.")
			return
		}
	}
}

// cmdRegister registers *this machine* as a device under the account.
//   - With --token: bind directly using a connect-token copied from the web
//     console (no login needed).
//   - Otherwise: requires a prior `anycode login`; calls the devices API to
//     create a device and obtain its connect-token.
func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	relay := fs.String("relay", "", "Relay/API base URL")
	token := fs.String("token", "", "Device connect-token from the web console")
	name := fs.String("name", "", "Device name")
	_ = fs.Parse(args)

	cfg := LoadConfig()
	if *relay != "" {
		cfg.RelayURL = *relay
	}
	devName := *name
	if devName == "" {
		devName, _ = os.Hostname()
		if devName == "" {
			devName = "我的开发机"
		}
	}

	if *token != "" {
		// Fetch device info using the token to get the real device ID
		parsed, status, err := apiRequest(cfg.RelayURL, "GET", "/api/devices/me", nil, *token)
		if err == nil && status == 200 {
			if dev, ok := parsed["device"].(map[string]interface{}); ok {
				if id, ok := dev["id"].(string); ok {
					cfg.DeviceID = id
				}
				if n, ok := dev["name"].(string); ok && n != "" {
					devName = n // override local default with cloud name
				}
			}
		}

		cfg.DeviceToken = *token
		cfg.DeviceName = devName
		if err := cfg.Save(); err != nil {
			fmt.Printf("Could not save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Device bound with token (DeviceID: %s).\n", cfg.DeviceID)
	} else {
		if cfg.Session == "" {
			fmt.Println("Not logged in. Run `anycode login` first, or use `anycode register --token <token>`.")
			os.Exit(1)
		}

		parsed, status, err := apiRequest(cfg.RelayURL, "POST", "/api/devices", map[string]string{
			"name": devName, "platform": runtime.GOOS,
		}, cfg.Session)
		if err != nil {
			fmt.Printf("Register failed: %v\n", err)
			os.Exit(1)
		}
		if status != 200 {
			fmt.Printf("Register failed: %v\n", apiError(parsed, status))
			os.Exit(1)
		}

		deviceToken, _ := parsed["token"].(string)
		if dev, ok := parsed["device"].(map[string]interface{}); ok {
			if id, ok := dev["id"].(string); ok {
				cfg.DeviceID = id
			}
		}
		if deviceToken == "" {
			fmt.Println("Register failed: no device token returned")
			os.Exit(1)
		}
		cfg.DeviceToken = deviceToken
		cfg.DeviceName = devName
		if err := cfg.Save(); err != nil {
			fmt.Printf("Could not save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Registered device %q.\n", devName)
	}

	// Auto-restart if the daemon is running in the background
	if pidFileExists() {
		fmt.Println("Detected background daemon running. Automatically restarting to apply new credentials...")
		cmdRestart([]string{})
	} else {
		fmt.Println("Run `anycode start` to go online.")
	}
}

func pidFileExists() bool {
	if _, err := os.Stat(pidFilePath()); err == nil {
		return true
	}
	return false
}

func cmdLogout() {
	cfg := LoadConfig()
	if cfg.Session != "" {
		_, _, _ = apiRequest(cfg.RelayURL, "POST", "/api/auth/logout", nil, cfg.Session)
	}
	cfg.Session = ""
	cfg.UserEmail = ""
	_ = cfg.Save()
	fmt.Println("Logged out.")
}

func apiError(parsed map[string]interface{}, status int) string {
	if parsed != nil {
		if msg, ok := parsed["error"].(string); ok && msg != "" {
			return msg
		}
	}
	return fmt.Sprintf("HTTP %d", status)
}
