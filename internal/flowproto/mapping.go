package flowproto

import (
	"fmt"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/graph"
)

// References are explicit JSON objects, not expressions or generated code.
// Examples: {"$ref":"input.target"}, {"$ref":"steps.inspect.text"}.
func resolve(value any, state graph.State) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		if ref, ok := v["$ref"]; ok {
			path, ok := ref.(string)
			if !ok || len(v) != 1 {
				return nil, fmt.Errorf("reference must contain only a string $ref")
			}
			return lookup(path, state)
		}
		out := map[string]any{}
		for key, item := range v {
			mapped, err := resolve(item, state)
			if err != nil {
				return nil, err
			}
			out[key] = mapped
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			mapped, err := resolve(item, state)
			if err != nil {
				return nil, err
			}
			out[i] = mapped
		}
		return out, nil
	default:
		return value, nil
	}
}

func lookup(path string, state graph.State) (any, error) {
	parts := strings.Split(path, ".")
	if len(parts) < 1 || (parts[0] != "input" && parts[0] != "steps") {
		return nil, fmt.Errorf("reference must start with input or steps: %s", path)
	}
	var current any = state["flow_"+parts[0]]
	for _, key := range parts[1:] {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("reference is not an object: %s", path)
		}
		current, ok = object[key]
		if !ok {
			return nil, fmt.Errorf("reference not found: %s", path)
		}
	}
	return current, nil
}
