package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

func firstString(obj map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if val, ok := obj[key]; ok {
			if s, ok := val.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func firstInt(obj map[string]interface{}, fallback int, keys ...string) int {
	for _, key := range keys {
		switch val := obj[key].(type) {
		case int:
			return val
		case int64:
			return int(val)
		case float64:
			return int(val)
		case json.Number:
			i, err := val.Int64()
			if err == nil {
				return int(i)
			}
		case string:
			i, err := strconv.Atoi(strings.TrimSpace(val))
			if err == nil {
				return i
			}
		}
	}
	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func int64Value(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
