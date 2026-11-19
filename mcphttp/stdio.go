package mcphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// stdioTransport owns its child for the entire connection, rather than tying
// the process to the discovery request's short-lived context. Close always
// reaps the child; each RPC independently obeys its caller's cancellation.
type stdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int]chan *rpcResponse
	done    chan struct{}
	exited  chan struct{}
	err     error
	once    sync.Once
}

func NewStdio(ctx context.Context, server, command string, args []string, env map[string]string) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	// Never mix child diagnostics with the JSON-RPC stdout stream.
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	tr := &stdioTransport{cmd: cmd, stdin: stdin, pending: make(map[int]chan *rpcResponse), done: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		defer close(tr.exited)
		tr.read(stdout)
		_ = cmd.Wait()
	}()
	c := &Client{server: server, protocol: protocolVersion, stdio: tr}
	if err := c.initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (s *stdioTransport) read(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var envelope struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			s.finish(fmt.Errorf("mcp stdio: invalid JSON: %w", err))
			return
		}
		if envelope.Method != "" || envelope.ID == 0 {
			continue
		}
		var response rpcResponse
		if err := json.Unmarshal(scanner.Bytes(), &response); err != nil {
			s.finish(err)
			return
		}
		s.mu.Lock()
		ch := s.pending[response.ID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- &response:
			default:
			}
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	s.finish(err)
}

func (s *stdioTransport) finish(err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		close(s.done)
		_ = s.stdin.Close()
		_ = s.cmd.Process.Kill()
	})
}

func (s *stdioTransport) Close() error {
	s.finish(errors.New("mcp stdio connection closed"))
	<-s.exited
	return nil
}

func (s *stdioTransport) roundTrip(ctx context.Context, request rpcRequest, expect bool) (*rpcResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { s.finish(ctx.Err()) })
	defer stop()
	ch := make(chan *rpcResponse, 1)
	if expect {
		s.mu.Lock()
		s.pending[request.ID] = ch
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.pending, request.ID); s.mu.Unlock() }()
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	_, err = s.stdin.Write(append(data, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	if !expect {
		return nil, nil
	}
	select {
	case response := <-ch:
		return response, nil
	case <-ctx.Done():
		// Closing the transport also unblocks a child that never reads stdin.
		s.finish(ctx.Err())
		return nil, ctx.Err()
	case <-s.done:
		select {
		case response := <-ch:
			return response, nil
		default:
		}
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return nil, fmt.Errorf("mcp stdio: %w", err)
	}
}
