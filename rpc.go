package main

import "encoding/json"

type RpcRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

type RpcResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result"`
	Error   *RpcError   `json:"error,omitempty"`
}

type RpcError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

type RpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

func makeResponse(id interface{}, result interface{}) RpcResponse {
	return RpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func makeError(id interface{}, code int, message string) RpcResponse {
	return RpcResponse{JSONRPC: "2.0", ID: id, Error: &RpcError{Code: code, Message: message}}
}

func makeNotification(method string, params interface{}) RpcNotification {
	return RpcNotification{JSONRPC: "2.0", Method: method, Params: params}
}

func parseID(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return nil
}

func getParams(params *json.RawMessage) map[string]interface{} {
	if params == nil {
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(*params, &m); err != nil {
		return nil
	}
	return m
}

func getParamString(params map[string]interface{}, key string) string {
	if params == nil {
		return ""
	}
	if v, ok := params[key].(string); ok {
		return v
	}
	return ""
}

func getOptionalParamString(params map[string]interface{}, key string) (*string, bool) {
	if params == nil {
		return nil, false
	}
	raw, ok := params[key]
	if !ok {
		return nil, false
	}
	if raw == nil {
		return nil, true
	}
	if v, ok := raw.(string); ok {
		return &v, true
	}
	return nil, true
}

func getParamInt(params map[string]interface{}, key string, fallback int) int {
	if params == nil {
		return fallback
	}
	if v, ok := params[key].(float64); ok {
		return int(v)
	}
	return fallback
}

func getParamBool(params map[string]interface{}, key string) bool {
	if params == nil {
		return false
	}
	if v, ok := params[key].(bool); ok {
		return v
	}
	return false
}

func getParamStringSlice(params map[string]interface{}, key string) []string {
	if params == nil {
		return nil
	}
	raw, ok := params[key]
	if !ok {
		return nil
	}
	values, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok || text == "" {
			continue
		}
		result = append(result, text)
	}
	return result
}
