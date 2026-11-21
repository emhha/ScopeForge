package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/emhha/scopeforge/internal/semantic"
)

// The fixture is produced by HF tokenizers 0.22.2 + ORT 1.24.1, using the
// pinned official INT8 model, attention-mask mean pooling and L2 normalization.
func TestE5MatchesReferenceIncludingUnicode(t *testing.T) {
	dir := filepath.Join("..", "..", "dist", "semantic")
	if _, err := os.Stat(filepath.Join(dir, "model.onnx")); err != nil {
		t.Skip("run make semantic-offline first")
	}
	model, err := openModel(semantic.Options{Model: filepath.Join(dir, "model.onnx"), Tokenizer: filepath.Join(dir, "tokenizer.json"), Runtime: filepath.Join(dir, semantic.RuntimeName())})
	if err != nil {
		t.Fatal(err)
	}
	defer model.close()
	data, err := os.ReadFile("testdata/e5-reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture []struct {
		Text   string    `json:"text"`
		IDs    []uint32  `json:"ids"`
		Vector []float64 `json:"vector"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixture {
		ids, _, err := model.tokenizer.EncodeErr(f.Text, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ids, f.IDs) {
			t.Fatalf("tokenizer mismatch for %q: %v vs %v", f.Text, ids, f.IDs)
		}
		v, err := model.embed(f.Text)
		if err != nil {
			t.Fatal(err)
		}
		var cosine, norm float64
		for i := range v {
			cosine += v[i] * f.Vector[i]
			norm += v[i] * v[i]
		}
		if cosine < 0.99999 || math.Abs(norm-1) > 1e-6 {
			t.Fatalf("reference mismatch for %q: cosine=%f norm=%f", f.Text, cosine, norm)
		}
	}
	if _, err := model.embed("query: " + strings.Repeat("权限验证 ", 600)); err != nil {
		t.Fatal("long input must truncate safely:", err)
	}
}
