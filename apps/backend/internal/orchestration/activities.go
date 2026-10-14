package orchestration

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/activity"
)

type SampleActivityInput struct {
	TaskID      string   `json:"task_id"`
	Steps       []string `json:"steps"`
	FailStep    int      `json:"fail_step"`
	StepDelayMs int      `json:"step_delay_ms,omitempty"`
}

type SampleActivityOutput struct {
	TaskID         string   `json:"task_id"`
	ProcessedSteps []string `json:"processed_steps"`
}

// SampleActivity executes each step, emits heartbeats, and recovers from previous heartbeat progress.
func SampleActivity(ctx context.Context, input SampleActivityInput) (SampleActivityOutput, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("Starting SampleActivity", "task_id", input.TaskID)

	startIndex := 0
	var processedSteps []string

	// Resume from previous heartbeat if worker was restarted or retrying
	if activity.HasHeartbeatDetails(ctx) {
		var lastCompletedIndex int
		if err := activity.GetHeartbeatDetails(ctx, &lastCompletedIndex); err == nil {
			startIndex = lastCompletedIndex + 1
			logger.Info("Resuming activity from heartbeat", "start_index", startIndex)
			for i := 0; i <= lastCompletedIndex && i < len(input.Steps); i++ {
				processedSteps = append(processedSteps, input.Steps[i])
			}
		}
	}

	for i := startIndex; i < len(input.Steps); i++ {
		select {
		case <-ctx.Done():
			logger.Warn("Activity context canceled", "task_id", input.TaskID)
			return SampleActivityOutput{}, ctx.Err()
		default:
		}

		stepName := input.Steps[i]
		logger.Info("Processing activity step", "step_index", i, "step_name", stepName)

		// Simulate activity step work
		stepDelay := 10 * time.Millisecond
		if input.StepDelayMs > 0 {
			stepDelay = time.Duration(input.StepDelayMs) * time.Millisecond
		}
		time.Sleep(stepDelay)

		// Simulate transient failure on specified step only on first attempt to prove retry & resume
		if input.FailStep > 0 && i == input.FailStep && activity.GetInfo(ctx).Attempt == 1 {
			logger.Warn("Simulating transient step failure on attempt 1", "step", i)
			return SampleActivityOutput{}, fmt.Errorf("transient failure at step %d for retry verification", i)
		}

		processedSteps = append(processedSteps, stepName)
		// Record progress heartbeat
		activity.RecordHeartbeat(ctx, i)
	}

	return SampleActivityOutput{
		TaskID:         input.TaskID,
		ProcessedSteps: processedSteps,
	}, nil
}
