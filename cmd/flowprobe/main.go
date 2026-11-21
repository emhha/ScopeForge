// flowprobe runs an isolated native GraphAgent/AgentTool experiment. It never
// connects to the production service, database, model provider or target URL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/emhha/scopeforge/internal/flowproto"
	"github.com/emhha/scopeforge/toolkit"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("flow", "internal/flowproto/testdata/assessment.json", "declarative flow JSON")
	input := flag.String("input", `{"target":"https://example.test"}`, "tool arguments as JSON")
	dot := flag.String("dot", "", "optional native Graphviz DOT output file")
	trace := flag.Bool("trace", false, "show native AgentTool streaming events on stderr")
	flag.Parse()
	file, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var def flowproto.Definition
	if err := decoder.Decode(&def); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("flow must contain exactly one JSON object")
	}
	inspect := toolkit.New("inspect", "Fixture: classifies the target string without making requests", map[string]any{"type": "object", "required": []string{"target"}, "properties": map[string]any{"target": map[string]any{"type": "string"}}}, func(_ context.Context, args json.RawMessage) (toolkit.Output, error) {
		var in struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return toolkit.Output{}, err
		}
		if strings.HasPrefix(in.Target, "https://") {
			return toolkit.Text("allowed"), nil
		}
		return toolkit.Text("denied"), nil
	})
	compiled, err := flowproto.Compile(def, map[string]tool.CallableTool{"inspect": inspect})
	if err != nil {
		return err
	}
	if *dot != "" {
		if err := os.WriteFile(*dot, []byte(compiled.Graph.DOT()), 0600); err != nil {
			return err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !*trace {
		result, err := compiled.Tool.Call(ctx, []byte(*input))
		if err != nil {
			return err
		}
		fmt.Println(toolkit.TextOf(result))
		return nil
	}
	reader, err := compiled.Tool.StreamableCall(ctx, []byte(*input))
	if err != nil {
		return err
	}
	defer reader.Close()
	var result string
	for {
		chunk, err := reader.Recv()
		if err == io.EOF {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Println(result)
			return nil
		}
		if err != nil {
			return err
		}
		if ev, ok := chunk.Content.(*event.Event); ok {
			if ev.Error != nil {
				return ev.Error
			}
			fmt.Fprintf(os.Stderr, "%s author=%s\n", ev.Object, ev.Author)
			for _, choice := range ev.Choices {
				if choice.Message.Role == "assistant" && choice.Message.Content != "" {
					result = choice.Message.Content
				}
			}
		} else {
			result = toolkit.TextOf(chunk.Content)
		}
	}
}
