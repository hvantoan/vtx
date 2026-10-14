# Temporal Go SDK — Worker, Determinism & Testing Patterns

**Date:** 2026-10-14 | **Context:** VTX-14 bootstrap, `apps/backend` Go module, `internal/orchestration`
**Sources:** docs.temporal.io (Go dev guide, error-handling, testing-suite), temporalio/samples-go. SDK >=1.30.

## 1. Determinism rules (Workflow code only)

Workflow functions replay event history on every worker poll; any non-deterministic call diverges and panics with `WorkflowTask failed: nondeterministic`. Forbidden inside Workflow definitions:

- `time.Now()`, `time.Sleep()`, timers, tickers -> use `workflow.Now(ctx)`, `workflow.Sleep(ctx)`, `workflow.NewTimer(ctx, d)`
- `math/rand`, `uuid.New()`, crypto reads -> wrap once in `workflow.SideEffect` (records result into history, replayed thereafter)
- Any I/O: net/http, DB (pgx), FS, `os.Getenv` (env varies per worker), process exec -> **all belongs in Activities** (contract section 2.1, AC-15)
- `go func`, `chan`, `select`, `sync.*` -> use `workflow.Go`, `workflow.NewChannel`, `workflow.NewSelector`, `workflow.NewMutex`
- `fmt.Println`/`log` -> `workflow.GetLogger(ctx)` (suppressed during replay)
- `for k := range map` affecting control flow — Go map order is random -> sort keys first
- `recover()` inside `defer` for SDK calls — panics re-panic during unwind; use explicit error checks
- Evolving workflow code on live executions -> guard changes with `workflow.GetVersion`; regression-check with `worker.NewWorkflowReplayer` against captured histories

```go
now := workflow.Now(ctx)                 // replayed from history
workflow.Sleep(ctx, 5*time.Second)       // durable timer
var runID string
workflow.SideEffect(ctx, func(workflow.Context) any {
    return uuid.NewString()              // executed once, result replayed
}).Get(&runID)
```

## 2. Sample workflow + activity: heartbeat, retry, worker-restart recovery

```go
// internal/orchestration/workflows.go
func ProcessItemsWorkflow(ctx workflow.Context, items []string) (int, error) {
    ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
        StartToCloseTimeout: 10 * time.Minute,
        HeartbeatTimeout:    30 * time.Second, // REQUIRED for heartbeat resume
        RetryPolicy: &temporal.RetryPolicy{
            InitialInterval:        time.Second,
            BackoffCoefficient:     2.0,
            MaximumInterval:        time.Minute,
            MaximumAttempts:        0, // unlimited; timeouts bound it
            NonRetryableErrorTypes: []string{"PermanentFailure"},
        },
    })
    var done int
    err := workflow.ExecuteActivity(ctx, ProcessItemsActivity, items).Get(ctx, &done)
    if err != nil {
        workflow.GetLogger(ctx).Error("activity failed", "error", err)
        return 0, err
    }
    return done, nil
}
```

```go
// internal/orchestration/activities.go — all real side effects live here
func ProcessItemsActivity(ctx context.Context, items []string) (int, error) {
    resume := 0
    if activity.HasHeartbeatDetails(ctx) { // set when a prior attempt died mid-run
        if err := activity.GetHeartbeatDetails(ctx, &resume); err != nil {
            return 0, err
        }
    }
    for i := resume; i < len(items); i++ {
        select {
        case <-ctx.Done(): // cancellation / heartbeat-timeout shutdown
            return i, ctx.Err()
        default:
        }
        if items[i] == "" {
            return 0, temporal.NewNonRetryableApplicationError("empty item", "PermanentFailure", nil)
        }
        // ... HTTP call / DB write / agent spawn (the ONLY place these are legal) ...
        activity.RecordHeartbeat(ctx, i+1) // next attempt resumes at i+1
    }
    return len(items), nil
}
```

**Worker-restart recovery (BS-05):** workflow state lives in server-side event history, not worker memory. On crash: workflow tasks redeliver to any polling worker and replay deterministically; an in-flight activity attempt times out per `StartToCloseTimeout`/`HeartbeatTimeout`, the `RetryPolicy` reschedules it, and the new attempt reads `GetHeartbeatDetails` to resume mid-list. Idempotency: heartbeats only skip already-committed work — external effects (e.g., DB upsert keyed by `activity.GetInfo(ctx).WorkflowExecution.ID`) must themselves be idempotent. Enforce `HeartbeatTimeout` always when long activities run, or a dead worker's attempt blocks until `StartToCloseTimeout`.

## 3. Testing with `go.temporal.io/sdk/testsuite`

Unit tests need no Temporal server; the env advances fake time (timers fire instantly).

```go
type WorkflowSuite struct {
    suite.Suite
    testsuite.WorkflowTestSuite
    env *testsuite.TestWorkflowEnvironment
}
func (s *WorkflowSuite) SetupTest()                  { s.env = s.NewTestWorkflowEnvironment() }
func (s *WorkflowSuite) AfterTest(_, _ string)       { s.env.AssertExpectations(s.T()) }
func TestWorkflowSuite(t *testing.T)                 { suite.Run(t, new(WorkflowSuite)) }

func (s *WorkflowSuite) TestSuccess() {
    s.env.OnActivity(ProcessItemsActivity, mock.Anything, []string{"a", "b"}).Return(2, nil)
    s.env.ExecuteWorkflow(ProcessItemsWorkflow, []string{"a", "b"})
    s.True(s.env.IsWorkflowCompleted())
    s.NoError(s.env.GetWorkflowError())
    var got int
    s.NoError(s.env.GetWorkflowResult(&got))
    s.Equal(2, got)
}

func (s *WorkflowSuite) TestNonRetryableFailsFast() {
    s.env.OnActivity(ProcessItemsActivity, mock.Anything, mock.Anything).
        Return(0, temporal.NewNonRetryableApplicationError("boom", "PermanentFailure", nil))
    s.env.ExecuteWorkflow(ProcessItemsWorkflow, []string{"x"})
    s.Error(s.env.GetWorkflowError())
}

// Activity-level: simulate a worker restart by injecting prior heartbeat details.
func (s *WorkflowSuite) TestActivityResumesFromHeartbeat() {
    env := s.NewTestActivityEnvironment()
    env.RegisterActivity(ProcessItemsActivity)
    env.SetHeartbeatDetails(2) // pretend attempt 1 finished items[0..1]
    var beats int
    env.SetOnActivityHeartbeatListener(func(_ *activity.Info, d converter.EncodedValues) { beats++ })
    val, err := env.ExecuteActivity(ProcessItemsActivity, []string{"a", "b", "c"})
    s.NoError(err)
    var n int
    s.NoError(val.Get(&n))
    s.Equal(3, n)    // resumed at index 2, still processed all 3
    s.Equal(1, beats) // only one heartbeat for item[2]
}
```

Also available: `env.RegisterDelayedCallback(fn, d)` for mid-run signals/queries, `env.CancelWorkflow()`, `env.SetTestTimeout(d)`, `env.SetWorkerOptions(worker.Options{BackgroundActivityContext: ctx})` to test cancellation. For BS-05 end-to-end proof, run a real dev-server (`testsuite.DevServer` / Docker Compose) test that SIGKILLs the worker mid-activity — the unit env mocks, it does not exercise replay.

## 4. Recommendation & risks

Adopt as written: Activities own all I/O; every long activity sets `HeartbeatTimeout` + progress heartbeats; workflows stay pure orchestration. Risks: (a) `MaximumAttempts: 0` retries forever — cap with workflow-level timeout for pipeline tasks; (b) `SideEffect` values are opaque in history — prefer fixed IDs generated client-side at workflow start when feasible; (c) testsuite mocks hide determinism bugs — add `worker.NewWorkflowReplayer` CI step on stored histories before Phase-1 pipelines. **Not covered:** Nexus/batching, worker versioning/build-IDs, interceptor-based determinism linting.
