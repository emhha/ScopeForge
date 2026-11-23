package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emhha/scopeforge/toolkit"
)

func TestWorkflowOutputReadRejectsSparseOversizeBeforeAllocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// A sparse file exercises a huge reported size without actually writing GBs.
	if err := f.Truncate(1 << 34); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, err := readWorkflowOutput(path, workflowOutputLimit); !errors.Is(err, errWorkflowOutputLimit) {
		t.Fatalf("oversized output was not rejected: %v", err)
	}
}

func TestWorkflowOutputCombinedBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if data, err := readWorkflowOutput(path, 5); err != nil || string(data) != "12345" {
		t.Fatalf("exact limit: %q %v", data, err)
	}
	if _, err := readWorkflowOutput(path, 4); !errors.Is(err, errWorkflowOutputLimit) {
		t.Fatal(err)
	}
}

func TestWorkflowOutputMonitorCancelsWriter(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr := filepath.Join(dir, "stdout"), filepath.Join(dir, "stderr")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := executeWithWorkflowOutputLimit(ctx, []string{stdout, stderr}, func(ctx context.Context) (any, error) {
		for _, path := range []string{stdout, stderr} {
			f, err := os.Create(path)
			if err != nil {
				return nil, err
			}
			err = f.Truncate(workflowOutputLimit/2 + 1)
			f.Close()
			if err != nil {
				return nil, err
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, errWorkflowOutputLimit) {
		t.Fatalf("writer wasn't stopped by combined output budget: %v", err)
	}
}

func TestWorkflowOutputLimitForNativeAndCustomCommands(t *testing.T) {
	s, _ := workflowTestServer(t)
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic", true: "custom"}[custom], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// Keep writing after a short burst. The command must be canceled, not
			// left sleeping until the workflow's five-minute timeout.
			command := "head -c 33554433 /dev/zero; sleep 30"
			var err error
			if custom {
				_, err = s.runWorkflowShell(ctx, command)
			} else {
				args, _ := json.Marshal(map[string]any{"program": "sh", "argv": []string{"-c", command}})
				_, err = s.commandWorkflowTool("").Call(ctx, args)
			}
			if !errors.Is(err, errWorkflowOutputLimit) {
				t.Fatalf("expected output limit, got %v", err)
			}
		})
	}
	// Small stderr output still survives, rather than being mistaken for failure.
	result, err := s.commandWorkflowTool("").Call(t.Context(), []byte(`{"program":"sh","argv":["-c","printf out; printf err >&2"]}`))
	if err != nil {
		t.Fatal(err)
	}
	data := result.(toolkit.Output).Data.(map[string]any)
	if data["stdout"] != "out" || data["stderr"] != "err" {
		t.Fatalf("lost output: %+v", data)
	}
}
