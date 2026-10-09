package coordinator

import (
	"encoding/json"
	"strconv"
	"strings"

	"ceraa/harness/tool"
)

// coerceToolArguments turns arguments that are valid JSON but the wrong shape
// into the shape the tool's schema asks for, and reports whether it changed
// anything.
//
// Syntax repair and key aliases already existed and cover the common failures.
// What they cannot cover is a model that sends the right keys with the wrong
// types: limit as "40", recursive as "true", a whole argument object wrapped in
// a JSON string. Each of those parses, so validation reads them as a schema
// violation and the model is told "must be a number" without ever being told
// why its JSON looked fine to it. Coercion removes that round trip for the
// conversions that have exactly one right answer.
//
// Deliberately narrow. Only conversions that cannot change meaning are
// attempted, and anything else is left for validation to reject: a string that
// is not a number stays a string, a scalar that cannot be an object is not
// wrapped into one, and an enum is never guessed at. A guess here would run a
// tool the model did not really ask for, which is worse than a clear error.
func coerceToolArguments(t tool.Tool, args map[string]any) bool {
	schema := t.Schema()
	if len(schema) == 0 {
		return false
	}
	properties, _ := schema["properties"].(map[string]any)
	changed := false
	for key, value := range args {
		property, ok := properties[key].(map[string]any)
		if !ok {
			continue
		}
		coerced, did := coerceValue(value, property)
		if did {
			args[key] = coerced
			changed = true
		}
	}
	return changed
}

func coerceValue(value any, property map[string]any) (any, bool) {
	// A JSON string holding an object where an object belongs is a double
	// encode: the model marshalled its arguments and the transport marshalled
	// them again. It is the one wrapping shape with an unambiguous answer.
	if text, ok := value.(string); ok {
		if inner, unwrapped := unwrapJSON(text, property); unwrapped {
			return inner, true
		}
	}
	kind, _ := property["type"].(string)
	switch kind {
	case "integer", "number":
		return coerceNumber(value, kind)
	case "boolean":
		return coerceBool(value)
	case "array":
		return coerceArray(value, property)
	}
	return value, false
}

// coerceNumber reads a number out of a string, and an integer out of a float
// the transport produced. Anything else is left alone so validation can report
// it as the type error it is.
func coerceNumber(value any, kind string) (any, bool) {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return value, false
		}
		if kind == "integer" {
			if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
				return float64(parsed), true
			}
			// A float that is whole is an integer the model wrote as one.
			if parsed, err := strconv.ParseFloat(text, 64); err == nil && parsed == float64(int64(parsed)) {
				return parsed, true
			}
			return value, false
		}
		if parsed, err := strconv.ParseFloat(text, 64); err == nil {
			return parsed, true
		}
		return value, false
	case float64:
		// JSON has one number type, so an integer field arrives as a float. A
		// whole number stays a float because the map is decoded generically and
		// validation compares against the schema, not against a Go type.
		if kind == "integer" && typed != float64(int64(typed)) {
			return typed, false
		}
		return value, false
	}
	return value, false
}

// coerceBool accepts the two spellings models actually emit. Anything else is
// left for validation, because "1" as false is a guess.
func coerceBool(value any) (any, bool) {
	text, ok := value.(string)
	if !ok {
		return value, false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return value, false
}

// coerceArray wraps a lone value where a one-item array belongs. Two models do
// this: one emitting a scalar for a single-element list, one emitting a
// comma-joined string for it. Both are unambiguous enough to correct, and both
// would otherwise cost a round trip to say "must be an array".
func coerceArray(value any, property map[string]any) (any, bool) {
	if items, ok := property["items"].(map[string]any); ok && items != nil {
		if _, isArray := property["items"].([]any); isArray {
			return value, false
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		return []any{typed}, true
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return value, false
		}
		var decoded []any
		if json.Unmarshal([]byte(text), &decoded) == nil && len(decoded) > 1 {
			return decoded, true
		}
		if json.Unmarshal([]byte(text), &decoded) == nil && len(decoded) == 1 {
			return decoded, true
		}
		return []any{typed}, true
	}
	return value, false
}

// unwrapJSON parses text that is itself JSON of the kind the property wants. It
// is the double-encode case and nothing else: text is only unwrapped when it
// parses cleanly and matches, so a path that happens to look like JSON is left
// alone unless it really is the object the property asked for.
func unwrapJSON(text string, property map[string]any) (any, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil, false
	}
	kind, _ := property["type"].(string)
	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, false
	}
	switch kind {
	case "object":
		if _, ok := decoded.(map[string]any); !ok {
			return nil, false
		}
	case "array":
		if _, ok := decoded.([]any); !ok {
			return nil, false
		}
	case "":
		return nil, false
	}
	return decoded, true
}
