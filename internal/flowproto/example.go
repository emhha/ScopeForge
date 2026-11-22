package flowproto

import (
	_ "embed"
	"encoding/json"
)

//go:embed testdata/assessment.json
var exampleJSON []byte

func ExampleDefinition() Definition {
	var def Definition
	if err := json.Unmarshal(exampleJSON, &def); err != nil {
		panic(err)
	}
	return def
}
