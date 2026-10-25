//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/agent"
	"github.com/vtx/platform/internal/observability"
)

// Create a mock agent host script in bash for testing the supervisor
func createMockAgentScript(t *testing.T) string {
	script := `#!/usr/bin/env bash
while IFS= read -r line; do
  if echo "$line" | grep -q '"method":"initialize"'; then
    id=$(echo "$line" | sed -E 's/.*"id":([0-9]+).*/\1/')
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocol_version\":\"1.0\",\"agent_type\":\"pi\",\"agent_version\":\"1.0.0\",\"capabilities\":{\"streaming\":true,\"cancellation\":true}}}"
  elif echo "$line" | grep -q '"method":"task/execute"'; then
    id=$(echo "$line" | sed -E 's/.*"id":([0-9]+).*/\1/')
    echo '{"jsonrpc":"2.0","method":"task/event","params":{"task_id":"t1","seq":1,"event_type":"progress","payload":"running step 1","timestamp":"2026-10-14T00:00:00Z"}}'
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"task_id\":\"t1\",\"status\":\"succeeded\",\"output\":\"Done successfully\"}}"
  elif echo "$line" | grep -q '"method":"shutdown"'; then
    exit 0
  fi
done
`
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "mock_agent.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)
	return scriptPath
}

func TestSupervisor_HandshakeAndExecution(t *testing.T) {
	scriptPath := createMockAgentScript(t)
	logger := observability.SetupLogger("error")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, scriptPath, nil, "pi", logger)
	require.NoError(t, err)
	defer sup.Close()

	// Execute task
	var result agent.TaskExecuteResult
	execParams := map[string]any{
		"task_id": "t1",
		"prompt":  "echo hello",
	}

	err = sup.Call(ctx, "task/execute", execParams, &result)
	require.NoError(t, err)
	assert.Equal(t, "t1", result.TaskID)
	assert.Equal(t, "succeeded", result.Status)
	assert.Equal(t, "Done successfully", result.Output)

	// Check event was received
	select {
	case event := <-sup.Events():
		assert.Equal(t, "t1", event.TaskID)
		assert.Equal(t, 1, event.Seq)
		assert.Equal(t, "progress", event.EventType)
	case <-time.After(1 * time.Second):
		t.Fatal("Expected task/event notification was not received")
	}
}

// newGitWorktree materializes the throwaway repo the IPC contract requires:
// workspace_path must be an existing dir and the fake agent commits into it.
func newGitWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out, err := exec.Command("git", "init", "-q", dir).CombinedOutput()
	require.NoError(t, err, string(out))
	return dir
}

// fakeAgentPath points the real host's CLI adapter at the scripted fixture
// (VTX_*_BIN env override) so the IPC + workspace + event path runs end-to-end
// without a real pi/omp agent.
func fakeAgentPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "..", "agent-runtime", "tests", "fixtures", "fake-agent.sh")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("fake agent fixture missing: %v", err)
	}
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	return abs
}

func TestSupervisor_RealNodeHost_Pi(t *testing.T) {
	hostJsPath := filepath.Join("..", "..", "..", "agent-runtime", "dist", "host.js")
	if _, err := os.Stat(hostJsPath); os.IsNotExist(err) {
		t.Skip("compiled agent runtime dist/host.js not found, skipping real node test")
	}
	t.Setenv("VTX_PI_BIN", fakeAgentPath(t))

	logger := observability.SetupLogger("error")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, "node", []string{hostJsPath}, "pi", logger)
	require.NoError(t, err)
	defer sup.Close()

	var result agent.TaskExecuteResult
	execParams := map[string]any{
		"task_id":        "real-pi-task",
		"prompt":         "Implement health check endpoint",
		"workspace_path": newGitWorktree(t),
	}

	err = sup.Call(ctx, "task/execute", execParams, &result)
	require.NoError(t, err)
	assert.Equal(t, "real-pi-task", result.TaskID)
	assert.Equal(t, "succeeded", result.Status)
	assert.Contains(t, result.Output, "done: committed")
}

func TestSupervisor_RealNodeHost_OMP(t *testing.T) {
	hostJsPath := filepath.Join("..", "..", "..", "agent-runtime", "dist", "host.js")
	if _, err := os.Stat(hostJsPath); os.IsNotExist(err) {
		t.Skip("compiled agent runtime dist/host.js not found, skipping real node test")
	}
	t.Setenv("VTX_OMP_BIN", fakeAgentPath(t))

	logger := observability.SetupLogger("error")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, "node", []string{hostJsPath}, "omp", logger)
	require.NoError(t, err)
	defer sup.Close()

	var result agent.TaskExecuteResult
	execParams := map[string]any{
		"task_id":        "real-omp-task",
		"prompt":         "Refactor interfaces",
		"workspace_path": newGitWorktree(t),
	}

	err = sup.Call(ctx, "task/execute", execParams, &result)
	require.NoError(t, err)
	assert.Equal(t, "real-omp-task", result.TaskID)
	assert.Equal(t, "succeeded", result.Status)
	assert.Contains(t, result.Output, "done: committed")
}

func TestSupervisor_GracefulShutdownNotification(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "shutdown_received.marker")

	script := `#!/usr/bin/env bash
while IFS= read -r line; do
  if echo "$line" | grep -q '"method":"initialize"'; then
    id=$(echo "$line" | sed -E 's/.*"id":([0-9]+).*/\1/')
    echo "{\"jsonrpc\":\"2.0\",\"id\":$id,\"result\":{\"protocol_version\":\"1.0\",\"agent_type\":\"pi\",\"agent_version\":\"1.0.0\",\"capabilities\":{}}}"
  elif echo "$line" | grep -q '"method":"shutdown"'; then
    touch "` + markerPath + `"
    exit 0
  fi
done
`
	scriptPath := filepath.Join(tmpDir, "shutdown_agent.sh")
	err := os.WriteFile(scriptPath, []byte(script), 0755)
	require.NoError(t, err)

	logger := observability.SetupLogger("error")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, scriptPath, nil, "pi", logger)
	require.NoError(t, err)

	// Call Close and assert that shutdown notification was received by process before exit
	err = sup.Close()
	require.NoError(t, err)

	assert.FileExists(t, markerPath, "Agent process must receive shutdown notification before termination")
}

func TestSupervisor_ConcurrentCancel(t *testing.T) {
	scriptPath := createMockAgentScript(t)
	logger := observability.SetupLogger("error")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, scriptPath, nil, "pi", logger)
	require.NoError(t, err)
	defer sup.Close()

	done := make(chan struct{})
	for i := range 10 {
		go func(id int) {
			_ = sup.CancelTask("task-concurrent", "user request")
			done <- struct{}{}
		}(i)
	}

	for range 10 {
		<-done
	}
}
