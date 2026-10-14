package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// JsonRpcRequest represents a JSON-RPC 2.0 request frame.
type JsonRpcRequest struct {
	JsonRpc string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// JsonRpcNotification represents a JSON-RPC 2.0 notification frame.
type JsonRpcNotification struct {
	JsonRpc string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// JsonRpcResponse represents a JSON-RPC 2.0 response frame.
type JsonRpcResponse struct {
	JsonRpc string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JsonRpcError   `json:"error,omitempty"`
}

type JsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type InitializeResult struct {
	ProtocolVersion string       `json:"protocol_version"`
	AgentType       string       `json:"agent_type"`
	AgentVersion    string       `json:"agent_version"`
	Capabilities    Capabilities `json:"capabilities"`
}

type Capabilities struct {
	Streaming    bool `json:"streaming"`
	Cancellation bool `json:"cancellation"`
}

type TaskExecuteResult struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

type TaskEvent struct {
	TaskID    string    `json:"task_id"`
	Seq       int       `json:"seq"`
	EventType string    `json:"event_type"`
	Payload   string    `json:"payload"`
	Timestamp time.Time `json:"timestamp"`
}

// Supervisor manages a child agent process over stdio.
type Supervisor struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Scanner
	stderr    io.ReadCloser
	reqID     int64
	logger    *slog.Logger
	respChans map[int64]chan JsonRpcResponse
	eventCh   chan TaskEvent
	mu        sync.Mutex
	closed    atomic.Bool
	pgid      int
}

// Spawn creates and initializes a new agent process with Unix process group isolation.
func Spawn(ctx context.Context, command string, args []string, agentType string, logger *slog.Logger) (*Supervisor, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// B4: Guarantee process group termination if ctx is cancelled during execution
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			pgid, err := syscall.Getpgid(cmd.Process.Pid)
			if err == nil && pgid > 0 {
				return syscall.Kill(-pgid, syscall.SIGKILL)
			}
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to open stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("failed to open stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdoutPipe.Close()
		return nil, fmt.Errorf("failed to open stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdoutPipe.Close()
		stderrPipe.Close()
		return nil, fmt.Errorf("failed to start agent process: %w", err)
	}

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}

	// B3: Configure scanner buffer to allow large payloads (up to 10 MB lines)
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

	s := &Supervisor{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    scanner,
		stderr:    stderrPipe,
		logger:    logger,
		respChans: make(map[int64]chan JsonRpcResponse),
		eventCh:   make(chan TaskEvent, 128),
		pgid:      pgid,
	}

	// Read loops
	go s.readStdoutLoop()
	go s.readStderrLoop()

	// Perform initialize handshake
	initReq := map[string]any{
		"protocol_version": "1.0",
		"client_name":      "vtx-supervisor",
		"client_version":   "1.0.0",
		"agent_type":       agentType,
	}

	var initResp InitializeResult
	if err := s.Call(ctx, "initialize", initReq, &initResp); err != nil {
		s.Close()
		return nil, fmt.Errorf("initialize handshake failed: %w", err)
	}

	logger.Info("Agent runtime initialized",
		"agent_type", initResp.AgentType,
		"protocol_version", initResp.ProtocolVersion,
		"pid", cmd.Process.Pid,
		"pgid", pgid,
	)

	return s, nil
}

func (s *Supervisor) Call(ctx context.Context, method string, params any, result any) error {
	if s.closed.Load() {
		return errors.New("agent supervisor is closed")
	}

	id := atomic.AddInt64(&s.reqID, 1)
	req := JsonRpcRequest{
		JsonRpc: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	respCh := make(chan JsonRpcResponse, 1)
	s.mu.Lock()
	s.respChans[id] = respCh
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.respChans, id)
		s.mu.Unlock()
	}()

	s.mu.Lock()
	_, err = fmt.Fprintf(s.stdin, "%s\n", string(payload))
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("failed to write request to stdin: %w", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-respCh:
		if !ok {
			return errors.New("supervisor closed while waiting for response")
		}
		if resp.Error != nil {
			return fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("failed to unmarshal rpc result: %w", err)
			}
		}
		return nil
	}
}

func (s *Supervisor) sendNotification(method string, params any) error {
	notif := JsonRpcNotification{
		JsonRpc: "2.0",
		Method:  method,
		Params:  params,
	}

	payload, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stdin == nil {
		return errors.New("stdin is nil or closed")
	}
	_, err = fmt.Fprintf(s.stdin, "%s\n", string(payload))
	return err
}

func (s *Supervisor) Notify(method string, params any) error {
	if s.closed.Load() {
		return errors.New("agent supervisor is closed")
	}
	return s.sendNotification(method, params)
}

func (s *Supervisor) Events() <-chan TaskEvent {
	return s.eventCh
}

func (s *Supervisor) CancelTask(taskID, reason string) error {
	return s.Notify("task/cancel", map[string]string{
		"task_id": taskID,
		"reason":  reason,
	})
}

func (s *Supervisor) readStdoutLoop() {
	defer func() {
		// B3: Drain pending channels and close event stream when loop terminates
		s.mu.Lock()
		for id, ch := range s.respChans {
			select {
			case ch <- JsonRpcResponse{
				JsonRpc: "2.0",
				ID:      id,
				Error: &JsonRpcError{
					Code:    -32000,
					Message: "agent process stdout closed",
				},
			}:
			default:
			}
		}
		clear(s.respChans)
		s.mu.Unlock()
		close(s.eventCh)
	}()

	for s.stdout.Scan() {
		line := s.stdout.Text()
		if len(line) == 0 {
			continue
		}

		// Check if it's a notification or response
		var generic map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &generic); err != nil {
			s.logger.Warn("Unparseable output on agent stdout", "line", line)
			continue
		}

		if _, hasMethod := generic["method"]; hasMethod {
			var notif JsonRpcNotification
			if err := json.Unmarshal([]byte(line), &notif); err == nil && notif.Method == "task/event" {
				var event TaskEvent
				paramBytes, _ := json.Marshal(notif.Params)
				if err := json.Unmarshal(paramBytes, &event); err == nil {
					select {
					case s.eventCh <- event:
					default:
					}
				}
			}
			continue
		}

		// It's a response
		var resp JsonRpcResponse
		if err := json.Unmarshal([]byte(line), &resp); err == nil {
			s.mu.Lock()
			ch, exists := s.respChans[resp.ID]
			s.mu.Unlock()
			if exists {
				select {
				case ch <- resp:
				default:
				}
			}
		}
	}

	if err := s.stdout.Err(); err != nil {
		s.logger.Error("Agent stdout scanner error", "error", err)
	}
}

func (s *Supervisor) readStderrLoop() {
	buf := make([]byte, 1024)
	for {
		n, err := s.stderr.Read(buf)
		if n > 0 {
			s.logger.Debug("agent stderr", "output", string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// Close gracefully terminates the agent process group (SIGTERM -> SIGKILL) to eliminate zombies.
func (s *Supervisor) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}

	// Send graceful shutdown notification before closing stdin
	_ = s.sendNotification("shutdown", map[string]any{})

	// Close stdin pipe to trigger EOF in child process
	s.mu.Lock()
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
	s.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		done <- s.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(500 * time.Millisecond):
		// Escalate to SIGTERM for process group
		if s.pgid > 0 {
			_ = syscall.Kill(-s.pgid, syscall.SIGTERM)
		}
	}

	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		// Escalate to SIGKILL for the entire process group
		if s.pgid > 0 {
			_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
		}
		select {
		case err := <-done:
			return err
		case <-time.After(1 * time.Second):
			return errors.New("timed out killing agent process group")
		}
	}
}
