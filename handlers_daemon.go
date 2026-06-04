package main

import "os"

func (s *Server) handleDaemonVersion(req RpcRequest, client *wsClient) (interface{}, error) {
	return map[string]string{"version": Version}, nil

}

func (s *Server) handleDaemonConfigRead(req RpcRequest, client *wsClient) (interface{}, error) {
	cfg := LoadConfig()
	return map[string]interface{}{
		"ok":    true,
		"proxy": cfg.Proxy,
	}, nil

}

func (s *Server) handleDaemonConfigWrite(req RpcRequest, client *wsClient) (interface{}, error) {
	params := getParams(req.Params)
	cfg := LoadConfig()
	proxy := getParamString(params, "proxy")
	cfg.Proxy = proxy
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	// Apply immediately to current process
	if cfg.Proxy != "" {
		os.Setenv("HTTP_PROXY", cfg.Proxy)
		os.Setenv("HTTPS_PROXY", cfg.Proxy)
		os.Setenv("ALL_PROXY", cfg.Proxy)
		os.Setenv("http_proxy", cfg.Proxy)
		os.Setenv("https_proxy", cfg.Proxy)
		os.Setenv("all_proxy", cfg.Proxy)
	} else {
		os.Unsetenv("HTTP_PROXY")
		os.Unsetenv("HTTPS_PROXY")
		os.Unsetenv("ALL_PROXY")
		os.Unsetenv("http_proxy")
		os.Unsetenv("https_proxy")
		os.Unsetenv("all_proxy")
	}
	return map[string]interface{}{"ok": true}, nil

}
