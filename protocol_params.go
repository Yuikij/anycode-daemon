package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// embeddedMethodsCatalog is the protocol method+param catalog compiled into the
// daemon binary. It is a verbatim copy of ../protocol/methods.json synced by
// build.sh (go:embed can't reference a parent directory). protocol/methods.json
// remains the single source of truth; protocol_params_test.go fails if the
// embedded copy drifts from it.
//
//go:embed methods.json
var embeddedMethodsCatalog []byte

// protocolValidator validates incoming RPC request params against the param
// specs declared in protocol/methods.json. It is the boundary-validation layer
// described in 协议对齐与稳定性收口计划.md §阶段一·2: rather than rewriting the
// daemon's `map[string]interface{}` internals, we keep the untyped maps but put
// a typed guard at the dispatch edge, driven by the single source of truth.
//
// Policy (intentionally lenient to avoid false rejections):
//   - Only declared params are type-checked; undeclared extra fields are allowed.
//   - `required` params must be present (and, for strings, non-empty).
//   - `passthrough` methods (codex dynamic) forward params verbatim and are not
//     validated here.
//   - Unknown methods are not validated (the router reports "unknown method").
type protocolValidator struct {
	methods map[string]methodParamSpec
}

type methodParamSpec struct {
	passthrough bool
	params      []paramSpec
}

type paramSpec struct {
	name     string
	typ      string
	required bool
}

// catalog mirror types used only for loading.
type protocolCatalogFile struct {
	Methods []struct {
		Name        string `json:"name"`
		Passthrough bool   `json:"passthrough"`
		Params      []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"params"`
	} `json:"methods"`
}

// mustLoadEmbeddedValidator parses the embedded catalog. The catalog is part of
// the build, so a parse failure is a programming/build error and should panic
// at startup rather than silently disabling validation.
func mustLoadEmbeddedValidator() *protocolValidator {
	v, err := newProtocolValidatorFromBytes(embeddedMethodsCatalog)
	if err != nil {
		panic(fmt.Sprintf("embedded protocol catalog invalid: %v", err))
	}
	return v
}

func newProtocolValidatorFromBytes(data []byte) (*protocolValidator, error) {
	var catalog protocolCatalogFile
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("parse method catalog: %w", err)
	}
	methods := make(map[string]methodParamSpec, len(catalog.Methods))
	for _, m := range catalog.Methods {
		spec := methodParamSpec{passthrough: m.Passthrough}
		for _, p := range m.Params {
			spec.params = append(spec.params, paramSpec{name: p.Name, typ: p.Type, required: p.Required})
		}
		methods[m.Name] = spec
	}
	return &protocolValidator{methods: methods}, nil
}

func (v *protocolValidator) validate(method string, rawParams *json.RawMessage) error {
	if v == nil {
		return nil
	}
	spec, ok := v.methods[method]
	if !ok || spec.passthrough || len(spec.params) == 0 {
		// Unknown method (router reports "unknown method"), passthrough method
		// (codex dynamic), or a method that declares no params: nothing to
		// validate here.
		return nil
	}

	params := map[string]interface{}{}
	if rawParams != nil && len(*rawParams) > 0 {
		if err := json.Unmarshal(*rawParams, &params); err != nil {
			return fmt.Errorf("invalid params for %s: not a JSON object", method)
		}
	}

	for _, p := range spec.params {
		value, present := params[p.name]
		if p.required {
			if !present || isMissingValue(value, p.typ) {
				return fmt.Errorf("missing required param %q for %s", p.name, method)
			}
		}
		if present && value != nil && !paramTypeMatches(value, p.typ) {
			return fmt.Errorf("param %q for %s must be %s", p.name, method, p.typ)
		}
	}
	return nil
}

func isMissingValue(value interface{}, typ string) bool {
	if value == nil {
		return true
	}
	if typ == "string" {
		s, ok := value.(string)
		return ok && s == ""
	}
	return false
}

func paramTypeMatches(value interface{}, typ string) bool {
	switch typ {
	case "any", "":
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "number":
		switch value.(type) {
		case float64, json.Number:
			return true
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "array":
		_, ok := value.([]interface{})
		return ok
	case "string[]":
		arr, ok := value.([]interface{})
		if !ok {
			return false
		}
		for _, item := range arr {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	case "object":
		_, ok := value.(map[string]interface{})
		return ok
	default:
		return true
	}
}
