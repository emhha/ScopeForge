package flowproto

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/emhha/scopeforge/toolkit"
)

var Operations = []string{"json.parse", "json.stringify", "lines", "join", "unique", "flatten", "select", "regex.extract", "base64.encode", "base64.decode", "read_text"}

func ValidOperation(op string) bool {
	for _, v := range Operations {
		if op == v {
			return true
		}
	}
	return false
}
func Transform(ctx context.Context, op string, args map[string]any) (any, error) {
	value := args["value"]
	text, _ := args["text"].(string)
	if text == "" {
		text, _ = value.(string)
	}
	if artifact, ok := args["artifact"]; ok {
		id, _ := artifact.(string)
		if ref, ok := artifact.(map[string]any); ok {
			id, _ = ref["id"].(string)
			if complete, ok := ref["complete"].(bool); ok && !complete && args["allow_partial"] != true {
				return nil, fmt.Errorf("artifact is incomplete")
			}
		}
		store := toolkit.ArtifactsFrom(ctx)
		if store == nil {
			return nil, fmt.Errorf("artifact store unavailable")
		}
		data, err := store.Read(ctx, id)
		if err != nil {
			return nil, err
		}
		if args["content_encoding"] == "gzip" {
			reader, err := gzip.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			data, err = io.ReadAll(io.LimitReader(reader, (32<<20)+1))
			if err != nil {
				return nil, err
			}
			if len(data) > 32<<20 {
				return nil, fmt.Errorf("decoded artifact exceeds 32 MiB")
			}
		}
		text = string(data)
		value = text
	}
	switch op {
	case "read_text":
		return text, nil
	case "json.parse":
		var out any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, err
		}
		return out, nil
	case "json.stringify":
		out, err := json.Marshal(value)
		return string(out), err
	case "lines":
		out := []any{}
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	case "join":
		array, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("join requires an array")
		}
		separator, ok := args["separator"].(string)
		if !ok {
			separator = "\n"
		}
		out := []string{}
		for _, item := range array {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("join requires string items")
			}
			out = append(out, s)
		}
		return strings.Join(out, separator), nil
	case "unique":
		array, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("unique requires array")
		}
		seen := map[string]bool{}
		out := []any{}
		for _, item := range array {
			raw, _ := json.Marshal(item)
			if !seen[string(raw)] {
				out = append(out, item)
				seen[string(raw)] = true
			}
		}
		return out, nil
	case "flatten":
		array, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("flatten requires array")
		}
		out := []any{}
		for _, item := range array {
			if list, ok := item.([]any); ok {
				out = append(out, list...)
			} else {
				out = append(out, item)
			}
		}
		return out, nil
	case "select":
		path, _ := args["path"].(string)
		if path == "" {
			return value, nil
		}
		for _, key := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
			object, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("select path is not an object")
			}
			var exists bool
			value, exists = object[strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")]
			if !exists {
				return nil, fmt.Errorf("select path not found")
			}
		}
		return value, nil
	case "regex.extract":
		pattern, _ := args["pattern"].(string)
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		out := []any{}
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			if len(match) > 1 {
				out = append(out, match[1])
			} else {
				out = append(out, match[0])
			}
		}
		return out, nil
	case "base64.encode":
		return base64.StdEncoding.EncodeToString([]byte(text)), nil
	case "base64.decode":
		data, err := base64.StdEncoding.DecodeString(text)
		return string(data), err
	default:
		return nil, fmt.Errorf("unknown transform %s", op)
	}
}
