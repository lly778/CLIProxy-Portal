package agy

import (
	"encoding/json"
)

// Validate the structural subset shared by function schemas and MCP. The
// client's own schema/grammar validation remains authoritative.
func validArguments(schema map[string]any, value any, depth int) bool {
	budget := 4096
	return validateArguments(schema, schema, value, depth, &budget)
}
func validateArguments(root, schema map[string]any, value any, depth int, budget *int) bool {
	*budget--
	if *budget < 0 {
		return false
	}

	if depth > 32 {
		return false
	}
	if schema == nil {
		return true
	}
	if ref, ok := schema["$ref"].(string); ok {
		target, exists := schemaPointer(root, ref)
		if !exists || !validateArguments(root, target, value, depth+1, budget) {
			return false
		}
	}
	if list, ok := schema["enum"].([]any); ok {
		found := false
		for _, x := range list {
			found = found || jsonValueEqual(x, value)
		}
		if !found {
			return false
		}
	}
	if constant, ok := schema["const"]; ok && !jsonValueEqual(constant, value) {
		return false
	}
	for _, kind := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := schema[kind].([]any); ok {
			matches := 0
			for _, x := range list {
				if s, ok := x.(map[string]any); ok && validateArguments(root, s, value, depth+1, budget) {
					matches++
				}
			}
			if kind == "anyOf" && matches == 0 || kind == "oneOf" && matches != 1 || kind == "allOf" && matches != len(list) {
				return false
			}
		}
	}
	matchType := func(typ string) bool {
		switch typ {
		case "null":
			return value == nil
		case "object":
			_, ok := value.(map[string]any)
			return ok
		case "array":
			_, ok := value.([]any)
			return ok
		case "string":
			_, ok := value.(string)
			return ok
		case "boolean":
			_, ok := value.(bool)
			return ok
		case "number", "integer":
			v, ok := numberRat(value)
			return ok && (typ != "integer" || v.IsInt())
		}
		return false
	}
	switch typ := schema["type"].(type) {
	case string:
		if !matchType(typ) {
			return false
		}
	case []any:
		ok := false
		for _, t := range typ {
			if s, yes := t.(string); yes {
				ok = ok || matchType(s)
			}
		}
		if !ok {
			return false
		}
	}
	if object, ok := value.(map[string]any); ok {
		properties, _ := schema["properties"].(map[string]any)
		if required, ok := schema["required"].([]any); ok {
			for _, k := range required {
				key, ok := k.(string)
				if !ok {
					return false
				}
				if _, exists := object[key]; !exists {
					return false
				}
			}
		}
		for key, v := range object {
			if p, exists := properties[key]; exists {
				if s, ok := p.(map[string]any); ok && !validateArguments(root, s, v, depth+1, budget) {
					return false
				}
			} else if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
				return false
			}
		}
	}
	if list, ok := value.([]any); ok {
		if s, ok := schema["items"].(map[string]any); ok {
			for _, v := range list {
				if !validateArguments(root, s, v, depth+1, budget) {
					return false
				}
			}
		}
	}
	return true
}
func normalizeSchema(s map[string]any) map[string]any {
	b, _ := json.Marshal(s)
	var out map[string]any
	_ = unmarshalExact(b, &out)
	return out
}
