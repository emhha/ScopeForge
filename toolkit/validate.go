package toolkit

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

var schemas sync.Map

type noExternalSchemas struct{}

func (noExternalSchemas) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference is disabled: %s", url)
}
func Validate(schema *tool.Schema, args []byte) error {
	if schema == nil {
		return nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	key := sha256.Sum256(raw)
	value, ok := schemas.Load(key)
	if !ok {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		compiler := jsonschema.NewCompiler()
		compiler.DefaultDraft(jsonschema.Draft2020)
		compiler.UseLoader(noExternalSchemas{})
		if err = compiler.AddResource("https://scopeforge.invalid/tool.json", document); err != nil {
			return err
		}
		compiled, err := compiler.Compile("https://scopeforge.invalid/tool.json")
		if err != nil {
			return err
		}
		value, _ = schemas.LoadOrStore(key, compiled)
	}
	if len(args) == 0 {
		args = []byte(`{}`)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return err
	}
	return value.(*jsonschema.Schema).Validate(document)
}
