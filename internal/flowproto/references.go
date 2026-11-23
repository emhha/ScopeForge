package flowproto

import (
	"fmt"
	"strings"
)

func destinations(n Node) []string {
	if n.Type == "parallel" {
		return n.Branches
	}
	if n.Type == "branch" {
		return []string{n.Condition.Then, n.Condition.Else}
	}
	if n.Type == "end" {
		return nil
	}
	return []string{n.Next}
}

// Compute which results must exist on every path. An all-of join contributes the
// union of its prerequisites; an ordinary conditional merge contributes their
// intersection. This prevents future/optional branch references and dead joins.
func validateGraphReferences(def Definition, nodes map[string]Node) error {
	predecessors := map[string][]string{}
	pending := map[string]int{}
	for _, n := range nodes {
		for _, next := range destinations(n) {
			predecessors[next] = append(predecessors[next], n.ID)
			pending[next]++
		}
	}
	queue := []string{def.Entry}
	dominates := map[string]map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		node := nodes[id]
		set := map[string]bool{}
		for i, parent := range predecessors[id] {
			if node.Type == "join" || i == 0 {
				for previous := range dominates[parent] {
					set[previous] = true
				}
			} else {
				for previous := range set {
					if !dominates[parent][previous] {
						delete(set, previous)
					}
				}
			}
		}
		set[id] = true
		dominates[id] = set
		for _, next := range destinations(node) {
			pending[next]--
			if pending[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	var check func(string, any) error
	check = func(id string, value any) error {
		switch v := value.(type) {
		case map[string]any:
			if raw, ok := v["$ref"]; ok {
				path, ok := raw.(string)
				if !ok || len(v) != 1 {
					return fmt.Errorf("node %s reference must contain only a string $ref", id)
				}
				parts := strings.Split(path, ".")
				if strings.HasPrefix(path, "#/") {
					parts = strings.Split(strings.TrimPrefix(path, "#/"), "/")
				}
				if len(parts) == 0 || (parts[0] != "input" && parts[0] != "steps") {
					return fmt.Errorf("node %s invalid reference %s", id, path)
				}
				if parts[0] == "steps" && len(parts) > 1 {
					target := parts[1]
					if _, exists := nodes[target]; !exists {
						return fmt.Errorf("node %s references missing node %s", id, target)
					}
					if target == id || !dominates[id][target] {
						return fmt.Errorf("node %s references %s before its result is guaranteed; add an explicit join/merge", id, target)
					}
				}
				return nil
			}
			for _, item := range v {
				if err := check(id, item); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range v {
				if err := check(id, item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, node := range nodes {
		if err := check(node.ID, node.Arguments); err != nil {
			return err
		}
		if err := check(node.ID, node.Output); err != nil {
			return err
		}
		if node.Condition != nil {
			if err := check(node.ID, map[string]any{"$ref": node.Condition.Ref}); err != nil {
				return err
			}
		}
	}
	var reachable func(string, string, map[string]bool) bool
	reachable = func(id, target string, seen map[string]bool) bool {
		if id == target {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		for _, next := range destinations(nodes[id]) {
			if reachable(next, target, seen) {
				return true
			}
		}
		return false
	}
	for _, join := range nodes {
		if join.Type != "join" {
			continue
		}
		for _, condition := range nodes {
			if condition.Type != "branch" {
				continue
			}
			// Only compare prerequisites controlled by this condition. A sibling
			// parallel branch is independent and must not make a local conditional
			// merge look like a join of mutually exclusive paths.
			required := []string{}
			for _, source := range join.From {
				if dominates[source][condition.ID] {
					required = append(required, source)
				}
			}
			for _, arm := range []string{condition.Condition.Then, condition.Condition.Else} {
				count := 0
				for _, source := range required {
					if reachable(arm, source, map[string]bool{}) {
						count++
					}
				}
				if count > 0 && count < len(required) {
					return fmt.Errorf("join %s waits for mutually exclusive conditional paths", join.ID)
				}
			}
		}
	}
	return nil
}
