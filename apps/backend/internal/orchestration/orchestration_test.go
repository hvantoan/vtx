package orchestration_test

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.temporal.io/sdk/testsuite"

	"github.com/vtx/platform/internal/orchestration"
)

type UnitTestSuite struct {
	suite.Suite
	testsuite.WorkflowTestSuite
	env *testsuite.TestWorkflowEnvironment
}

func (s *UnitTestSuite) SetupTest() {
	s.env = s.NewTestWorkflowEnvironment()
}

func (s *UnitTestSuite) TearDownTest() {
	s.env.AssertExpectations(s.T())
}

func (s *UnitTestSuite) Test_SampleWorkflow_Success() {
	input := orchestration.SampleWorkflowInput{
		TaskID: "task-test-123",
		Steps:  []string{"init", "lint", "build", "test"},
	}

	expectedOutput := orchestration.SampleActivityOutput{
		TaskID:         "task-test-123",
		ProcessedSteps: []string{"init", "lint", "build", "test"},
	}

	// Mock activity
	s.env.OnActivity(orchestration.SampleActivity, mock.Anything, mock.Anything).Return(expectedOutput, nil)

	s.env.ExecuteWorkflow(orchestration.SampleWorkflow, input)

	s.True(s.env.IsWorkflowCompleted())
	s.NoError(s.env.GetWorkflowError())

	var result orchestration.SampleWorkflowOutput
	s.NoError(s.env.GetWorkflowResult(&result))
	s.Equal("task-test-123", result.TaskID)
	s.Equal([]string{"init", "lint", "build", "test"}, result.CompletedSteps)
}

func (s *UnitTestSuite) Test_SampleActivity_DirectExecution() {
	actEnv := s.NewTestActivityEnvironment()
	actEnv.RegisterActivity(orchestration.SampleActivity)

	input := orchestration.SampleActivityInput{
		TaskID: "act-test-1",
		Steps:  []string{"step-a", "step-b"},
	}

	val, err := actEnv.ExecuteActivity(orchestration.SampleActivity, input)
	s.NoError(err)

	var output orchestration.SampleActivityOutput
	s.NoError(val.Get(&output))
	s.Equal([]string{"step-a", "step-b"}, output.ProcessedSteps)
}

func (s *UnitTestSuite) Test_SampleActivity_HeartbeatResume() {
	actEnv := s.NewTestActivityEnvironment()
	actEnv.RegisterActivity(orchestration.SampleActivity)

	// Simulate that step 0 was completed previously and stored in heartbeat details
	actEnv.SetHeartbeatDetails(0)

	input := orchestration.SampleActivityInput{
		TaskID: "act-test-resume",
		Steps:  []string{"step-0", "step-1", "step-2"},
	}

	val, err := actEnv.ExecuteActivity(orchestration.SampleActivity, input)
	s.NoError(err)

	var output orchestration.SampleActivityOutput
	s.NoError(val.Get(&output))
	// Should process step-1 and step-2 while retaining step-0
	s.Equal([]string{"step-0", "step-1", "step-2"}, output.ProcessedSteps)
}

func TestUnitTestSuite(t *testing.T) {
	suite.Run(t, new(UnitTestSuite))
}
