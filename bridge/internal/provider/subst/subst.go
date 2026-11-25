// Package subst implements the v1 template substitution shared by the
// config-driven providers (board T-019): "{{name}}" placeholders filled
// from the exec params plus the task ID.
package subst

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Expand replaces every "{{name}}" placeholder in tpl with the string
// form of the corresponding variable. Strings are inserted as-is, numbers
// and booleans textually, objects/arrays as compact JSON. An unknown or
// null variable is an error — a silently-empty substitution would produce
// confusing requests downstream.
func Expand(tpl string, vars map[string]any) (string, error) {
	var b []byte
	for {
		start := indexOf(tpl, "{{")
		if start < 0 {
			return string(b) + tpl, nil
		}
		rel := indexOf(tpl[start:], "}}")
		if rel < 0 {
			return string(b) + tpl, nil
		}
		end := start + rel
		b = append(b, tpl[:start]...)
		name := trimSpace(tpl[start+2 : end])
		v, ok := vars[name]
		if !ok || v == nil {
			return "", fmt.Errorf("missing placeholder variable %q", name)
		}
		b = append(b, stringify(v)...)
		tpl = tpl[end+2:]
	}
}

// Vars builds the substitution variable set from a task ID and the raw
// params object: the task ID is exposed as {{task_id}}, every params
// entry as {{<key>}}. params must be a JSON object.
func Vars(taskID string, params []byte) (map[string]any, error) {
	var parsed map[string]any
	if len(params) > 0 {
		if err := json.Unmarshal(params, &parsed); err != nil || parsed == nil {
			return nil, fmt.Errorf("params must be a JSON object")
		}
	}
	vars := make(map[string]any, len(parsed)+1)
	for k, v := range parsed {
		vars[k] = v
	}
	vars["task_id"] = taskID
	return vars, nil
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool, int, int64, float64:
		return fmt.Sprintf("%v", t)
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(bytes.TrimRight(buf.Bytes(), "\n"))
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
