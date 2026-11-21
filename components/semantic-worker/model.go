package main

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/daulet/tokenizers"
	"github.com/emhha/scopeforge/internal/semantic"
	ort "github.com/yalue/onnxruntime_go"
)

type localModel struct {
	tokenizer *tokenizers.Tokenizer
	session   *ort.DynamicAdvancedSession
}

func openModel(options semantic.Options) (*localModel, error) {
	if options.Model == "" || options.Tokenizer == "" || options.Runtime == "" {
		return nil, errors.New("local model is not installed")
	}
	tk, err := tokenizers.FromFile(options.Tokenizer)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	loaded := false
	defer func() {
		if !loaded {
			tk.Close()
		}
	}()
	ort.SetSharedLibraryPath(options.Runtime)
	if err = ort.InitializeEnvironment(); err != nil {
		return nil, err
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	defer opts.Destroy()
	if err = opts.SetIntraOpNumThreads(1); err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	if err = opts.SetInterOpNumThreads(1); err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSession(options.Model, []string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, opts)
	if err != nil {
		ort.DestroyEnvironment()
		return nil, err
	}
	loaded = true
	return &localModel{tokenizer: tk, session: session}, nil
}

func (m *localModel) close() { m.tokenizer.Close(); m.session.Destroy(); ort.DestroyEnvironment() }

func (m *localModel) embed(text string) ([]float64, error) {
	if !strings.HasPrefix(text, "query: ") && !strings.HasPrefix(text, "passage: ") {
		return nil, errors.New("E5 query/passage prefix required")
	}
	ids, _, err := m.tokenizer.EncodeErr(text, true)
	if err != nil {
		return nil, err
	}
	if len(ids) > 512 {
		ids = append(append([]uint32{}, ids[:511]...), 2)
	} // XLM-R </s>
	if len(ids) == 0 {
		return nil, errors.New("empty token sequence")
	}
	input := make([]int64, len(ids))
	mask := make([]int64, len(ids))
	types := make([]int64, len(ids))
	for i, id := range ids {
		input[i] = int64(id)
		mask[i] = 1
	}
	shape := ort.NewShape(1, int64(len(ids)))
	a, err := ort.NewTensor(shape, input)
	if err != nil {
		return nil, err
	}
	defer a.Destroy()
	b, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer b.Destroy()
	c, err := ort.NewTensor(shape, types)
	if err != nil {
		return nil, err
	}
	defer c.Destroy()
	outputs := []ort.Value{nil}
	if err = m.session.Run([]ort.Value{a, b, c}, outputs); err != nil {
		return nil, err
	}
	defer outputs[0].Destroy()
	tensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, errors.New("expected float32 hidden states")
	}
	data := tensor.GetData()
	if len(data) != len(ids)*384 {
		return nil, errors.New("unexpected E5 output shape")
	}
	vector := make([]float64, 384)
	for i, v := range data {
		vector[i%384] += float64(v) / float64(len(ids))
	}
	var norm float64
	for _, v := range vector {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errors.New("invalid model output")
	}
	for i := range vector {
		vector[i] /= norm
	}
	return vector, nil
}
