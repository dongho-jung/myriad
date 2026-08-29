package myriad

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Record is an open JSON object because lifecycle participants update disjoint
// metadata while a task is live. Every load still requires Myriad's one current
// schema; no legacy state is accepted.
type Record map[string]any

func cloneRecord(value Record) Record {
	payload, _ := json.Marshal(value)
	var result Record
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	_ = decoder.Decode(&result)
	return result
}

func stringValue(record Record, key string) string {
	value, _ := record[key].(string)
	return value
}

func optionalString(record Record, key string) (string, bool) {
	value, ok := record[key].(string)
	return value, ok && value != ""
}

func boolValue(record Record, key string, fallback bool) bool {
	value, ok := record[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func intValue(value any) (int, bool) {
	switch current := value.(type) {
	case int:
		return current, true
	case int64:
		return int(current), true
	case float64:
		if current == float64(int(current)) {
			return int(current), true
		}
	case json.Number:
		parsed, err := strconv.Atoi(current.String())
		return parsed, err == nil
	}
	return 0, false
}

func recordMap(record Record, key string) Record {
	switch value := record[key].(type) {
	case Record:
		return value
	case map[string]any:
		return Record(value)
	default:
		return nil
	}
}

func recordSlice(record Record, key string) []any {
	value, _ := record[key].([]any)
	return value
}

func requireString(record Record, key string) (string, error) {
	value := stringValue(record, key)
	if value == "" {
		return "", fail("task has no %s", strings.ReplaceAll(key, "_", " "))
	}
	return value, nil
}

func describe(value any) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(payload)
}
