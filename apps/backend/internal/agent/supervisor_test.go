package agent_test

import (
	"context"
	"os"
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

func TestSupervisor_RealNodeHost_Pi(t *testing.T) {
	hostJsPath := filepath.Join("..", "..", "..", "agent-runtime", "dist", "host.js")
	if _, err := os.Stat(hostJsPath); os.IsNotExist(err) {
		t.Skip("compiled agent runtime dist/host.js not found, skipping real node test")
	}

	logger := observability.SetupLogger("error")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, "node", []string{hostJsPath}, "pi", logger)
	require.NoError(t, err)
	defer sup.Close()

	var result agent.TaskExecuteResult
	execParams := map[string]any{
		"task_id": "real-pi-task",
		"prompt":  "Implement health check endpoint",
	}

	err = sup.Call(ctx, "task/execute", execParams, &result)
	require.NoError(t, err)
	assert.Equal(t, "real-pi-task", result.TaskID)
	assert.Equal(t, "succeeded", result.Status)
	assert.Contains(t, result.Output, "Pi agent successfully executed")
}

func TestSupervisor_RealNodeHost_OMP(t *testing.T) {
	hostJsPath := filepath.Join("..", "..", "..", "agent-runtime", "dist", "host.js")
	if _, err := os.Stat(hostJsPath); os.IsNotExist(err) {
		t.Skip("compiled agent runtime dist/host.js not found, skipping real node test")
	}

	logger := observability.SetupLogger("error")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sup, err := agent.Spawn(ctx, "node", []string{hostJsPath}, "omp", logger)
	require.NoError(t, err)
	defer sup.Close()

	var result agent.TaskExecuteResult
	execParams := map[string]any{
		"task_id": "real-omp-task",
		"prompt":  "Refactor interfaces",
	}

	err = sup.Call(ctx, "task/execute", execParams, &result)
	require.NoError(t, err)
	assert.Equal(t, "real-omp-task", result.TaskID)
	assert.Equal(t, "succeeded", result.Status)
	assert.Contains(t, result.Output, "OMP agent successfully completed")
}
