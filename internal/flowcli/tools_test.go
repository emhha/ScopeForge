package flowcli

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestReconDefinitionCompilesWithoutExecutingCommands(t *testing.T) {
	pool := map[string]tool.CallableTool{}
	for _, item := range Tools(func(context.Context, []byte) (any, error) {
		t.Fatal("validation must not run installed commands")
		return nil, nil
	}) {
		pool[item.Declaration().Name] = item
	}
	raw, err := os.ReadFile("../flowproto/testdata/domain-recon.json")
	if err != nil {
		t.Fatal(err)
	}
	var def flowproto.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	if _, err := flowproto.Compile(def, pool); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string]string{
		"subfinder":        `{"domain":"example.com"}`,
		"amass":            `{"domain":"example.com"}`,
		"merge_subdomains": `{"domain":"example.com","lists":[["a.example.com"],["a.example.com","b.example.com"]]}`,
		"httpx":            `{"hosts":["a.example.com","b.example.com"]}`,
		"ehole":            `{"urls":["https://a.example.com"]}`,
	} {
		if err := toolkit.Validate(pool[name].Declaration().InputSchema, []byte(args)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestMergeDomainsFiltersAndDeduplicates(t *testing.T) {
	want := []string{"a.example.com", "b.example.com"}
	got := names("b.example.com\nA.example.com\na.example.com\nevil-example.com\nfoo.other.com\n[log]\n", "example.com")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, value := range []string{"https://example.com", "example.com;touch /tmp/x", "-example.com", "a..example.com"} {
		if _, err := domain(value); err == nil {
			t.Fatalf("accepted invalid domain %q", value)
		}
	}
}
