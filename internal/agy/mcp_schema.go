package agy

import "encoding/json"

// The official CLI projects MCP enums into Google's string-only enum field.
// Publish incompatible enums as a description while retaining the original
// client schema for tools/call validation and session identity.
func projectSchemaEnums(original map[string]any) map[string]any {
	if original == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	projected := normalizeSchema(original)
	var visit func(map[string]any)
	visit = func(schema map[string]any) {
		if values, ok := schema["enum"].([]any); ok {
			compatible := len(values) > 0
			for _, value := range values {
				text, ok := value.(string)
				compatible = compatible && ok && text != ""
			}
			if !compatible {
				delete(schema, "enum")
				encoded, _ := json.Marshal(values)
				hint := "Allowed values are restricted by the client schema."
				if len(encoded) <= 2048 {
					hint = "Allowed values: " + string(encoded) + "."
				}
				description, _ := schema["description"].(string)
				if description != "" {
					description += "\n"
				}
				schema["description"] = description + hint
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if children, ok := schema[key].(map[string]any); ok {
				for _, child := range children {
					if node, ok := child.(map[string]any); ok {
						visit(node)
					}
				}
			}
		}
		for _, key := range []string{"items", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "not", "if", "then", "else", "contains", "propertyNames"} {
			if node, ok := schema[key].(map[string]any); ok {
				visit(node)
			}
		}
		for _, key := range []string{"allOf", "anyOf", "oneOf", "prefixItems", "items"} {
			if children, ok := schema[key].([]any); ok {
				for _, child := range children {
					if node, ok := child.(map[string]any); ok {
						visit(node)
					}
				}
			}
		}
	}
	visit(projected)
	return projected
}
