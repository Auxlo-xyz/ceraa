package tool

import (
	"fmt"
	"math"
	"strings"
)

func ValidateArguments(schema map[string]any, args map[string]any) error {
	return validateValue("$", args, schema)
}

func validateValue(path string, value any, schema map[string]any) error {
	if schema == nil {
		return nil
	}
	if raw, ok := schema["type"].(string); ok && raw != "" {
		if err := validateType(path, value, raw); err != nil {
			return err
		}
	}
	if enum, ok := schema["enum"].([]string); ok && len(enum) > 0 {
		matched := false
		text := fmt.Sprint(value)
		for _, candidate := range enum {
			if text == candidate {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s must be one of %s", path, strings.Join(enum, ", "))
		}
	}
	switch typed := value.(type) {
	case map[string]any:
		if err := validateObject(path, typed, schema); err != nil {
			return err
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for i, item := range typed {
			if err := validateValue(fmt.Sprintf("%s[%d]", path, i), item, items); err != nil {
				return err
			}
		}
	}
	return validateBounds(path, value, schema)
}

func validateType(path string, value any, kind string) error {
	switch kind {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s must be an object", path)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s must be an array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s must be a string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be a boolean", path)
		}
	case "number":
		if !isNumber(value) {
			return fmt.Errorf("%s must be a number", path)
		}
	case "integer":
		if !isInteger(value) {
			return fmt.Errorf("%s must be an integer", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s must be null", path)
		}
	}
	return nil
}

func validateObject(path string, value map[string]any, schema map[string]any) error {
	properties, _ := schema["properties"].(map[string]any)
	required := requiredFields(schema)
	for _, name := range required {
		if _, ok := value[name]; !ok {
			return fmt.Errorf("%s.%s is required", path, name)
		}
	}
	additional, configured := schema["additionalProperties"].(bool)
	if !configured {
		additional = false
	}
	if !additional {
		for name := range value {
			if _, known := properties[name]; !known {
				return fmt.Errorf("%s.%s is not allowed", path, name)
			}
		}
	}
	for name, child := range value {
		property, _ := properties[name].(map[string]any)
		if err := validateValue(path+"."+name, child, property); err != nil {
			return err
		}
	}
	return nil
}

func requiredFields(schema map[string]any) []string {
	switch values := schema["required"].(type) {
	case []string:
		return values
	case []any:
		fields := make([]string, 0, len(values))
		for _, value := range values {
			if field, ok := value.(string); ok {
				fields = append(fields, field)
			}
		}
		return fields
	default:
		return nil
	}
}

func validateBounds(path string, value any, schema map[string]any) error {
	if text, ok := value.(string); ok {
		if minimum, ok := numberValue(schema["minLength"]); ok && float64(len([]rune(text))) < minimum {
			return fmt.Errorf("%s must contain at least %.0f characters", path, minimum)
		}
		if maximum, ok := numberValue(schema["maxLength"]); ok && float64(len([]rune(text))) > maximum {
			return fmt.Errorf("%s must contain at most %.0f characters", path, maximum)
		}
	}
	if items, ok := value.([]any); ok {
		if minimum, ok := numberValue(schema["minItems"]); ok && float64(len(items)) < minimum {
			return fmt.Errorf("%s must contain at least %.0f items", path, minimum)
		}
		if maximum, ok := numberValue(schema["maxItems"]); ok && float64(len(items)) > maximum {
			return fmt.Errorf("%s must contain at most %.0f items", path, maximum)
		}
	}
	if isNumber(value) {
		number, _ := numberValue(value)
		if minimum, ok := numberValue(schema["minimum"]); ok && number < minimum {
			return fmt.Errorf("%s must be at least %.0f", path, minimum)
		}
		if maximum, ok := numberValue(schema["maximum"]); ok && number > maximum {
			return fmt.Errorf("%s must be at most %.0f", path, maximum)
		}
	}
	return nil
}

func isNumber(value any) bool {
	switch value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return true
	default:
		return false
	}
}

func isInteger(value any) bool {
	switch number := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return true
	case float32:
		return float64(number) == math.Trunc(float64(number))
	case float64:
		return number == math.Trunc(number)
	default:
		return false
	}
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}
