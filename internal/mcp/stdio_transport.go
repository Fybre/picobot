package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/picobot/internal/config"
)

const (
	defaultShutdownTimeout = 5 * time.Second
)

// StdioTransport implements Transport using stdio (subprocess).
type StdioTransport struct {
	cmd       *exec.Cmd
	stdin     *bufio.Writer
	stdinPipe *os.File
	stdout    *bufio.Reader
	stderr    *bufio.Reader
	mu        sync.Mutex
	closed    atomic.Bool
}

// NewStdioTransport creates a new stdio-based transport.
func NewStdioTransport(cfg config.MCPServerConfig) (*StdioTransport, error) {
	cmd := exec.Command(cfg.Command, cfg.Args...)

	// Set environment variables
	if len(cfg.Env) > 0 {
		env := os.Environ()
		for k, v := range cfg.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	// Get the file from the pipe for closing later
	stdinFile, ok := stdin.(*os.File)
	if !ok {
		stdinFile = nil
	}

	transport := &StdioTransport{
		cmd:       cmd,
		stdin:     bufio.NewWriter(stdin),
		stdinPipe: stdinFile,
		stdout:    bufio.NewReader(stdout),
		stderr:    bufio.NewReader(stderr),
	}

	// Start stderr logger
	go transport.logStderr()

	return transport, nil
}

// logStderr logs stderr output from the MCP server.
func (t *StdioTransport) logStderr() {
	scanner := bufio.NewScanner(t.stderr)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			log.Printf("[MCP Server] %s", line)
		}
	}
	if err := scanner.Err(); err != nil && !t.closed.Load() {
		log.Printf("[MCP] Stderr scanner error: %v", err)
	}
}

// Call sends a JSON-RPC request and returns the result.
func (t *StdioTransport) Call(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if t.closed.Load() {
		return nil, fmt.Errorf("transport is closed")
	}

	// Marshal params
	var paramsRaw json.RawMessage
	var err error
	if params != nil {
		paramsRaw, err = json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
	}

	req := request{
		JSONRPC: "2.0",
		ID:      nextRequestID(),
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create per-call state
	type callResult struct {
		resp *response
		err  error
	}
	callDone := make(chan callResult, 1)

	// Start response reader goroutine
	go func(reqID int, done chan<- callResult) {
		resp, err := t.readResponse(reqID)
		select {
		case done <- callResult{resp: resp, err: err}:
		default:
			// Timeout occurred, discard
		}
	}(req.ID, callDone)

	// Send request
	t.mu.Lock()
	_, err = t.stdin.Write(append(data, '\n'))
	if err != nil {
		t.mu.Unlock()
		return nil, fmt.Errorf("failed to write request: %w", err)
	}
	err = t.stdin.Flush()
	t.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("failed to flush request: %w", err)
	}

	// Wait for response or timeout
	select {
	case result := <-callDone:
		if result.err != nil {
			return nil, result.err
		}
		if result.resp.Error != nil {
			return nil, result.resp.Error
		}
		return result.resp.Result, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("MCP request timeout: %w", ctx.Err())
	}
}

// readResponse reads and parses a JSON-RPC response with the given ID.
func (t *StdioTransport) readResponse(expectedID int) (*response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var resp response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			log.Printf("[MCP] Failed to parse response: %v", err)
			continue
		}

		if resp.ID == expectedID {
			return &resp, nil
		}

		log.Printf("[MCP] Unexpected response ID: got %d, want %d", resp.ID, expectedID)
	}
}

// SendNotification sends a JSON-RPC notification.
func (t *StdioTransport) SendNotification(ctx context.Context, method string, params interface{}) error {
	if t.closed.Load() {
		return fmt.Errorf("transport is closed")
	}

	var paramsRaw json.RawMessage
	if params != nil {
		var err error
		paramsRaw, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("failed to marshal params: %w", err)
		}
	}

	req := request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	_, err = t.stdin.Write(append(data, '\n'))
	if err != nil {
		return fmt.Errorf("failed to write notification: %w", err)
	}
	return t.stdin.Flush()
}

// Close gracefully shuts down the transport.
func (t *StdioTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}

	if t.stdinPipe != nil {
		_ = t.stdinPipe.Close()
	}

	if t.cmd != nil && t.cmd.Process != nil {
		// Create a channel to signal when Wait() completes
		done := make(chan struct{})
		go func() {
			_ = t.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Process exited gracefully
		case <-time.After(defaultShutdownTimeout):
			// Timeout - force kill
			if err := t.cmd.Process.Kill(); err != nil {
				log.Printf("[MCP] Failed to kill process: %v", err)
			}
			// Wait for the goroutine to complete after kill
			<-done
		}
	}

	return nil
}
