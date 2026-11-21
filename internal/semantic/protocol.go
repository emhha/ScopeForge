// Package semantic defines the private stdio protocol for the optional native
// component. The inference runtime remains isolated from the main application.
package semantic

import "trpc.group/trpc-go/trpc-agent-go/memory"

const ProtocolVersion = 2

type Options struct {
	Protocol   int    `json:"protocol"`
	Database   string `json:"database"`
	Dimensions int    `json:"dimensions"`
	Model      string `json:"model,omitempty"`
	Tokenizer  string `json:"tokenizer,omitempty"`
	Runtime    string `json:"runtime,omitempty"`
}

type Request struct {
	Op      string                `json:"op"`
	Options *Options              `json:"options,omitempty"`
	Scope   memory.UserKey        `json:"scope"`
	Entry   *memory.Entry         `json:"entry,omitempty"`
	ID      string                `json:"id,omitempty"`
	Text    string                `json:"text,omitempty"`
	Vector  []float64             `json:"vector,omitempty"`
	Search  *memory.SearchOptions `json:"search,omitempty"`
}

type Response struct {
	ModelLoaded bool            `json:"model_loaded"`
	Error       string          `json:"error,omitempty"`
	Entries     []*memory.Entry `json:"entries,omitempty"`
	Vector      []float64       `json:"vector,omitempty"`
	Protocol    int             `json:"protocol,omitempty"`
}
