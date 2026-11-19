package toolkit

import (
	"testing"

	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNativeToolSchemaRejectsInvalidInputAndExternalReferences(t *testing.T) {
	schema := &tool.Schema{Type: "object", Required: []string{"id"}, Properties: map[string]*tool.Schema{"id": {Type: "integer"}}}
	for _, args := range []string{`{}`, `{"id":"wrong"}`, `null`} {
		if Validate(schema, []byte(args)) == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	if err := Validate(schema, []byte(`{"id":42}`)); err != nil {
		t.Fatal(err)
	}
	if Validate(&tool.Schema{Ref: "file:///etc/passwd"}, []byte(`{}`)) == nil {
		t.Fatal("loaded external schema")
	}
}
