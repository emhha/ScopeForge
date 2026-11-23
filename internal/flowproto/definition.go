// Package flowproto compiles declarative workflows into native tRPC-Agent-Go
// graphs. The server exposes them as registered custom tools and inline runs.
package flowproto

import (
	"fmt"
	"regexp"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type Definition struct {
	SchemaVersion int            `json:"schema_version,omitempty"`
	OutputSchema  map[string]any `json:"output_schema,omitempty"`
	Name          string         `json:"name"`
	Description   string         `json:"description"`
	Version       int            `json:"version"`
	InputSchema   map[string]any `json:"input_schema"`
	Entry         string         `json:"entry"`
	Nodes         []Node         `json:"nodes"`
}

type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type Node struct {
	Branches  []string       `json:"branches,omitempty"`
	From      []string       `json:"from,omitempty"`
	Operation string         `json:"operation,omitempty"`
	Position  *Position      `json:"position,omitempty"`
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
		case "parallel":
			if len(n.Branches) < 2 || n.Next != "" {
				return fmt.Errorf("parallel %s requires at least two branches and no next", n.ID)
			}
			seen := map[string]bool{}
			for _, branch := range n.Branches {
				if seen[branch] {
					return fmt.Errorf("parallel %s has duplicate branch %s", n.ID, branch)
				}
				seen[branch] = true
			}
		case "join":
			if len(n.From) < 2 || n.Next == "" {
				return fmt.Errorf("join %s requires from and next", n.ID)
			}
		case "start":
			starts++
			if n.Next == "" {
				return fmt.Errorf("start %s requires next", n.ID)
			}
		case "tool", "transform":
			if n.Type == "tool" && (tools[n.Tool] == nil || tools[n.Tool].Declaration().Name != n.Tool) {
				return fmt.Errorf("node %s references unavailable tool %q", n.ID, n.Tool)
			}
			if n.Type == "transform" && !ValidOperation(n.Operation) {
				return fmt.Errorf("unknown transform %q", n.Operation)
			}
			_, wholeReference := n.Arguments["$ref"]
			if n.Type == "tool" && !(wholeReference && len(n.Arguments) == 1) {
				if schema := tools[n.Tool].Declaration().InputSchema; schema != nil {
					for _, key := range schema.Required {
						if _, exists := n.Arguments[key]; !exists {
							if property := schema.Properties[key]; property != nil && property.Default != nil && (property.Type == "string" || property.Type == "integer" || property.Type == "number" || property.Type == "boolean") {
								continue
							}
							return fmt.Errorf("node %s missing required argument %s", n.ID, key)
						}
					}
				}
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
		if n.Type == "parallel" {
			next = append(next, n.Branches...)
		} else if n.Type == "branch" {
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
	for _, n := range d.Nodes {
		if n.Type == "join" {
			seen := map[string]bool{}
			for _, from := range n.From {
				if seen[from] || nodes[from].Next != n.ID {
					return fmt.Errorf("join %s has invalid predecessor %s", n.ID, from)
				}
				seen[from] = true
			}
			for _, other := range d.Nodes {
				for _, target := range destinations(other) {
					if target == n.ID && !seen[other.ID] {
						return fmt.Errorf("join %s missing predecessor %s; connect conditional/parallel branches through execution nodes first", n.ID, other.ID)
					}
				}
			}
		}
	}
	if err := validateParallelPaths(nodes); err != nil {
		return err
	}
	return validateGraphReferences(d, nodes)
}

// A fork must meet at an explicit all-of join before reaching an end. Otherwise
// the native graph can run a shared downstream command twice or finish while a
// sibling is still running. Conditional merges remain ordinary edges.
func validateParallelPaths(nodes map[string]Node) error {
	post := map[string]map[string]bool{}
	var after func(string) map[string]bool
	after = func(id string) map[string]bool {
		if result := post[id]; result != nil {
			return result
		}
		result := map[string]bool{}
		for i, next := range destinations(nodes[id]) {
			child := after(next)
			if i == 0 {
				for key := range child {
					result[key] = true
				}
			} else {
				for key := range result {
					if !child[key] {
						delete(result, key)
					}
				}
			}
		}
		result[id] = true
		post[id] = result
		return result
	}
	for _, fork := range nodes {
		if fork.Type != "parallel" {
			continue
		}
		common := after(fork.ID)
		join := ""
		for id := range common {
			if nodes[id].Type == "join" && (join == "" || after(id)[join]) {
				join = id
			}
		}
		if join == "" {
			return fmt.Errorf("parallel %s requires an explicit join reached by every branch before ending", fork.ID)
		}
		owners := map[string]int{}
		for index, branch := range fork.Branches {
			seen := map[string]bool{}
			var visit func(string) error
			visit = func(id string) error {
				if id == join || seen[id] {
					return nil
				}
				seen[id] = true
				if owner, exists := owners[id]; exists && owner != index {
					return fmt.Errorf("parallel %s branches meet at %s before join %s; connect branch ends to the join first", fork.ID, id, join)
				}
				owners[id] = index
				for _, next := range destinations(nodes[id]) {
					if err := visit(next); err != nil {
						return err
					}
				}
				return nil
			}
			if err := visit(branch); err != nil {
				return err
			}
		}
	}
	return nil
}
