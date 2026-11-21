package semantic

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
	"time"
)

// Client serializes requests. A timed out inference terminates the native
// process, so an unhealthy runtime cannot indefinitely block the Go server.
type Client struct {
	modelLoaded bool
	gate        chan struct{}
	mu          sync.Mutex
	path        string
	options     Options
	cmd         *exec.Cmd
	in          io.WriteCloser
	out         *bufio.Reader
	lastUsed    time.Time
	closed      bool
	done        chan struct{}
}

func NewClient(path string, options Options) *Client {
	c := &Client{path: path, options: options, gate: make(chan struct{}, 1), done: make(chan struct{})}
	c.gate <- struct{}{}
	go c.reap()
	return c
}

func (c *Client) stopLocked() {
	if c.cmd != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		_ = c.in.Close()
		c.cmd, c.in, c.out = nil, nil, nil
		c.modelLoaded = false
	}
}

func (c *Client) reap() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
			select {
			case <-c.gate:
				c.mu.Lock()
				if time.Since(c.lastUsed) > 5*time.Minute {
					c.stopLocked()
				}
				c.mu.Unlock()
				c.gate <- struct{}{}
			default:
			}
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	c.stopLocked()
	return nil
}

func (c *Client) ModelLoaded() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.modelLoaded }

func (c *Client) Running() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.cmd != nil }

func exchange(in io.Writer, out *bufio.Reader, req Request) (Response, error) {
	if err := json.NewEncoder(in).Encode(req); err != nil {
		return Response{}, err
	}
	line, err := out.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	if len(line) > 32<<20 {
		return Response{}, errors.New("semantic response too large")
	}
	var res Response
	if err = json.Unmarshal(line, &res); err != nil {
		return res, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	select {
	case <-ctx.Done():
		return Response{}, ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Response{}, errors.New("semantic component closed")
	}
	if c.cmd == nil {
		cmd := exec.Command(c.path)
		cmd.Env = []string{"TOKENIZERS_PARALLELISM=false", "RAYON_NUM_THREADS=1", "OMP_NUM_THREADS=1"}
		// The component has no reason to inherit chat API keys or proxy credentials.
		for _, key := range []string{"PATH", "SystemRoot", "WINDIR", "TMPDIR", "TMP", "TEMP"} {
			if v, ok := os.LookupEnv(key); ok {
				cmd.Env = append(cmd.Env, key+"="+v)
			}
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			c.mu.Unlock()
			return Response{}, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			in.Close()
			c.mu.Unlock()
			return Response{}, err
		}
		if err = cmd.Start(); err != nil {
			in.Close()
			c.mu.Unlock()
			return Response{}, fmt.Errorf("start semantic component: %w", err)
		}
		c.cmd, c.in, c.out = cmd, in, bufio.NewReader(out)
		// Initialization is also covered by the request timeout below.
	}
	in, out := c.in, c.out
	c.mu.Unlock()
	type result struct {
		res Response
		err error
	}
	finished := make(chan result, 1)
	go func() {
		if req.Op != "init" {
			res, err := exchange(in, out, Request{Op: "init", Options: &c.options})
			if err != nil || res.Protocol != ProtocolVersion {
				if err == nil {
					err = errors.New("semantic protocol mismatch")
				}
				finished <- result{res, err}
				return
			}
		}
		res, err := exchange(in, out, req)
		finished <- result{res, err}
	}()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.stopLocked()
		c.mu.Unlock()
		<-finished
		return Response{}, ctx.Err()
	case r := <-finished:
		c.mu.Lock()
		c.lastUsed = time.Now()
		c.modelLoaded = r.res.ModelLoaded
		if r.err != nil {
			c.stopLocked()
		}
		c.mu.Unlock()
		return r.res, r.err
	}
}
