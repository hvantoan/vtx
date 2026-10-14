package orchestration

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

type SampleWorkflowInput struct {
	TaskID      string   `json:"task_id"`
	Steps       []string `json:"steps"`
	FailStep    int      `json:"fail_step"`  // Optional step index to fail for testing retries
	StepDelayMs int      `json:"step_delay_ms,omitempty"`
}

type SampleWorkflowOutput struct {
	TaskID         string   `json:"task_id"`
	CompletedSteps []string `json:"completed_steps"`
	TotalDuration  string   `json:"total_duration"`
}

// SampleWorkflow demonstrates a strictly deterministic Temporal workflow.
// All side effects and external communications are delegated to Activities.
func SampleWorkflow(ctx workflow.Context, input SampleWorkflowInput) (SampleWorkflowOutput, error) {
	logger := workflow.GetLogger(ctx)
	startTime := workflow.Now(ctx)
	logger.Info("Starting SampleWorkflow", "task_id", input.TaskID)

	// Activity options with timeout and retry policy
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		HeartbeatTimeout:    5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    100 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    1 * time.Second,
			MaximumAttempts:    3,
			NonRetryableErrorTypes: []string{
				"NonRetryableError",
			},
		},
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptions)

	actInput := SampleActivityInput{
		TaskID:      input.TaskID,
		Steps:       input.Steps,
		FailStep:    input.FailStep,
		StepDelayMs: input.StepDelayMs,
	}

	var actOutput SampleActivityOutput
	err := workflow.ExecuteActivity(ctx, SampleActivity, actInput).Get(ctx, &actOutput)
	if err != nil {
		logger.Error("SampleActivity failed", "error", err)
		return SampleWorkflowOutput{}, err
	}

	duration := workflow.Now(ctx).Sub(startTime).String()
	logger.Info("SampleWorkflow completed successfully", "task_id", input.TaskID, "duration", duration)

	return SampleWorkflowOutput{
		TaskID:         input.TaskID,
		CompletedSteps: actOutput.ProcessedSteps,
		TotalDuration:  duration,
	}, nil
}
