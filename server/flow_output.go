package server

import (
	"context"
	"errors"
	"io"
	"os"
	"time"
)

const workflowOutputLimit int64 = 32 << 20

var errWorkflowOutputLimit = errors.New("command output exceeds 32 MiB")

// Read at most the remaining output budget plus one byte, even if a file grows
// between Stat and Read. Never read an unbounded file before checking its size.
func readWorkflowOutput(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, errWorkflowOutputLimit
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errWorkflowOutputLimit
	}
	return data, nil
}

// Stop long-running writers when their redirected stdout/stderr exceed the
// combined budget. The bounded read also checks short bursts between polls.
func executeWithWorkflowOutputLimit(ctx context.Context, paths []string, execute func(context.Context) (any, error)) (any, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				var size int64
				for _, path := range paths {
					info, err := os.Stat(path)
					if errors.Is(err, os.ErrNotExist) {
						continue // redirection has not opened this stream yet
					}
					if err != nil {
						cancel(err)
						return
					}
					size += info.Size()
				}
				if size > workflowOutputLimit {
					cancel(errWorkflowOutputLimit)
					return
				}
			}
		}
	}()
	result, err := execute(ctx)
	close(stop)
	<-done
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	return result, err
}
