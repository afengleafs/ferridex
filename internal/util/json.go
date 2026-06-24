package util

import (
	"encoding/json"
	"fmt"
)

func Truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d bytes total)", len(s))
}

func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func GetString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func GetMap(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

func MustMarshalRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(b)
}

func MarshalJSONLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"type":"error","message":"failed to marshal event"}`
	}
	return string(b)
}
