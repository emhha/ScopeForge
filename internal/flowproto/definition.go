// Package flowproto experiments with compiling declarative flows into native
// tRPC-Agent-Go graphs. It is not registered with the production tool catalog.
package flowproto

import (
	"fmt"
	"regexp"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type Definition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Version     int            `json:"version"`
	InputSchema map[string]any `json:"input_schema"`
	Entry       string         `json:"entry"`
	Nodes       []Node         `json:"nodes"`
}

type Node struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"` // start, tool, branch, end
	Tool      string         `json:"tool,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Next      string         `json:"next,omitempty"`
	Condition *Condition     `json:"condition,omitempty"`
	Output    any            `json:"output,omitempty"`
}

type Condition struct {
	Ref    string `json:"ref"`
	Equals any    `json:"equals"`
	Then   string `json:"then"`
	Else   string `json:"else"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,58}$`)

func (d Definition) validate(tools map[string]tool.CallableTool) error {
	if !identifier.MatchString(d.Name) || d.Version < 1 {
		return fmt.Errorf("flow requires a valid name and positive version")
	}
	if d.InputSchema["type"] != "object" {
		return fmt.Errorf("input_schema must describe an object")
	}
	if len(d.Nodes) == 0 || len(d.Nodes) > 64 {
		return fmt.Errorf("flow must contain 1–64 nodes")
	}
	nodes := map[string]Node{}
	starts := 0
	for _, n := range d.Nodes {
		if !identifier.MatchString(n.ID) {
			return fmt.Errorf("invalid node ID %q", n.ID)
		}
		if _, ok := nodes[n.ID]; ok {
			return fmt.Errorf("duplicate node %q", n.ID)
		}
		nodes[n.ID] = n
		switch n.Type {
		case "start":
			starts++
			if n.Next == "" {
				return fmt.Errorf("start %s requires next", n.ID)
			}
		case "tool":
			if tools[n.Tool] == nil || tools[n.Tool].Declaration().Name != n.Tool {
				return fmt.Errorf("node %s references unavailable tool %q", n.ID, n.Tool)
			}
			if n.Next == "" {
				return fmt.Errorf("tool %s requires next", n.ID)
			}
		case "branch":
			if n.Condition == nil || n.Condition.Ref == "" || n.Condition.Then == "" || n.Condition.Else == "" || n.Next != "" {
				return fmt.Errorf("invalid branch %s", n.ID)
			}
		case "end":
			if n.Next != "" {
				return fmt.Errorf("end %s cannot have next", n.ID)
			}
		default:
			return fmt.Errorf("unknown node type %q", n.Type)
		}
	}
	if starts != 1 || nodes[d.Entry].Type != "start" {
		return fmt.Errorf("entry must be the only start node")
	}
	// This validates the editable definition only. Scheduling, state propagation,
	// routing and cancellation are performed by the native graph runtime.
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		n, ok := nodes[id]
		if !ok {
			return fmt.Errorf("unknown destination %q", id)
		}
		if visiting[id] {
			return fmt.Errorf("cycles are not supported by this prototype: %s", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		next := []string{}
		if n.Type == "branch" {
			next = append(next, n.Condition.Then, n.Condition.Else)
		} else if n.Type != "end" {
			next = append(next, n.Next)
		}
		for _, dst := range next {
			if err := visit(dst); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	if err := visit(d.Entry); err != nil {
		return err
	}
	if len(visited) != len(nodes) {
		return fmt.Errorf("flow contains unreachable nodes")
	}
	return nil
}
