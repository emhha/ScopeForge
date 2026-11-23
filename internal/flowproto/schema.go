package flowproto

// DefinitionSchema is shared by Agent-facing tools and the editor catalog.
func DefinitionSchema() map[string]any {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	return map[string]any{"type": "object", "required": []string{"name", "input_schema", "entry", "nodes"}, "properties": map[string]any{
		"name": str("工作流名，字母开头，只包含字母数字下划线短横线"), "description": str("用途"), "version": map[string]any{"type": "integer", "minimum": 1, "default": 1},
		"input_schema": map[string]any{"type": "object", "description": "运行输入的 JSON Schema，type 必须是 object"}, "output_schema": map[string]any{"type": "object"}, "entry": str("唯一 start 节点 ID"),
		"nodes": map[string]any{"type": "array", "minItems": 1, "maxItems": 64, "items": map[string]any{"type": "object", "required": []string{"id", "type"}, "properties": map[string]any{
			"id": str("唯一节点 ID"), "type": map[string]any{"type": "string", "enum": []string{"start", "tool", "transform", "branch", "parallel", "join", "end"}}, "tool": str("workflow_catalog 中允许的工具名"), "arguments": map[string]any{"type": "object", "description": "参数 JSON，支持 {\"$ref\":\"input.domain\"} 或 {\"$ref\":\"#/steps/fetch/data/body_artifact\"}"},
			"operation": map[string]any{"type": "string", "enum": Operations}, "next": str("下一节点 ID；end/branch/parallel 不设置"),
			"branches": map[string]any{"type": "array", "minItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string"}, "description": "parallel 并行入口；各分支必须先在共同的 join 汇合，再执行公共下游或结束"}, "from": map[string]any{"type": "array", "minItems": 2, "uniqueItems": true, "items": map[string]any{"type": "string"}, "description": "join 等待全部前驱，前驱 next 必须指向该 join；不能等待互斥条件出口"},
			"condition": map[string]any{"type": "object", "required": []string{"ref", "equals", "then", "else"}, "properties": map[string]any{"ref": str("input 或 steps 引用"), "equals": map[string]any{}, "then": str("相等时节点"), "else": str("不相等时节点")}}, "output": map[string]any{"description": "end 节点结果，支持嵌套引用"},
		}}},
	}}
}
