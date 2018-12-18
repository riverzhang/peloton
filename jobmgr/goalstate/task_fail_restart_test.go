package goalstate

import (
	"context"
	"fmt"
	"testing"

	mesosv1 "code.uber.internal/infra/peloton/.gen/mesos/v1"
	pbjob "code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	pbtask "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/models"

	goalstatemocks "code.uber.internal/infra/peloton/common/goalstate/mocks"
	cachedmocks "code.uber.internal/infra/peloton/jobmgr/cached/mocks"
	jobmgrcommon "code.uber.internal/infra/peloton/jobmgr/common"
	storemocks "code.uber.internal/infra/peloton/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

type TaskFailRetryTestSuite struct {
	suite.Suite
	ctrl *gomock.Controller

	taskStore  *storemocks.MockTaskStore
	jobFactory *cachedmocks.MockJobFactory

	taskGoalStateEngine *goalstatemocks.MockEngine
	jobGoalStateEngine  *goalstatemocks.MockEngine
	goalStateDriver     *driver

	jobID      *peloton.JobID
	instanceID uint32
	updateID   *peloton.UpdateID

	taskEnt      *taskEntity
	cachedJob    *cachedmocks.MockJob
	cachedUpdate *cachedmocks.MockUpdate
	cachedTask   *cachedmocks.MockTask

	jobRuntime      *pbjob.RuntimeInfo
	taskRuntime     *pbtask.RuntimeInfo
	lostTaskRuntime *pbtask.RuntimeInfo

	mesosTaskID string
}

func TestTaskFailRetryStart(t *testing.T) {
	suite.Run(t, new(TaskFailRetryTestSuite))
}

func (suite *TaskFailRetryTestSuite) TearDownTest() {
	suite.ctrl.Finish()
}

func (suite *TaskFailRetryTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.taskStore = storemocks.NewMockTaskStore(suite.ctrl)
	suite.jobGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.taskGoalStateEngine = goalstatemocks.NewMockEngine(suite.ctrl)
	suite.jobFactory = cachedmocks.NewMockJobFactory(suite.ctrl)
	suite.cachedJob = cachedmocks.NewMockJob(suite.ctrl)
	suite.cachedTask = cachedmocks.NewMockTask(suite.ctrl)
	suite.cachedUpdate = cachedmocks.NewMockUpdate(suite.ctrl)
	suite.goalStateDriver = &driver{
		jobEngine:  suite.jobGoalStateEngine,
		taskEngine: suite.taskGoalStateEngine,
		taskStore:  suite.taskStore,
		jobFactory: suite.jobFactory,
		mtx:        NewMetrics(tally.NoopScope),
		cfg:        &Config{},
	}
	suite.goalStateDriver.cfg.normalize()
	suite.jobID = &peloton.JobID{Value: uuid.NewRandom().String()}
	suite.updateID = &peloton.UpdateID{Value: uuid.NewRandom().String()}
	suite.instanceID = uint32(0)
	suite.taskEnt = &taskEntity{
		jobID:      suite.jobID,
		instanceID: suite.instanceID,
		driver:     suite.goalStateDriver,
	}
	suite.mesosTaskID = fmt.Sprintf("%s-%d-%d", suite.jobID.GetValue(), suite.instanceID, 1)
	suite.taskRuntime = &pbtask.RuntimeInfo{
		MesosTaskId:   &mesosv1.TaskID{Value: &suite.mesosTaskID},
		State:         pbtask.TaskState_FAILED,
		GoalState:     pbtask.TaskState_SUCCEEDED,
		ConfigVersion: 1,
		Message:       "testFailure",
		Reason:        mesosv1.TaskStatus_REASON_COMMAND_EXECUTOR_FAILED.String(),
	}
	suite.lostTaskRuntime = &pbtask.RuntimeInfo{
		MesosTaskId:   &mesosv1.TaskID{Value: &suite.mesosTaskID},
		State:         pbtask.TaskState_LOST,
		GoalState:     pbtask.TaskState_SUCCEEDED,
		ConfigVersion: 1,
		Message:       "Agent peloton-mesos-agent0 removed",
		Reason:        mesosv1.TaskStatus_REASON_AGENT_REMOVED.String(),
	}
	suite.jobRuntime = &pbjob.RuntimeInfo{
		UpdateID: suite.updateID,
	}
}

// TestTaskFailNoRetry tests when restart policy is set to 0 for batch task
func (suite *TaskFailRetryTestSuite) TestTaskFailNoRetry() {

	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 0,
		},
	}

	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(&taskConfig, &models.ConfigAddOn{}, nil)

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.NoError(err)
}

// TestTaskFailRetry tests retry for failed task
func (suite *TaskFailRetryTestSuite) TestTaskFailRetry() {
	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 3,
		},
	}
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedJob.EXPECT().
		ID().Return(suite.jobID)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(&taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			runtimeDiff := runtimeDiffs[suite.instanceID]
			suite.True(
				runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetJobType().Return(pbjob.JobType_BATCH)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.NoError(err)
}

// TestLostTaskRetry tests retry for lost task
func (suite *TaskFailRetryTestSuite) TestLostTaskRetry() {
	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 3,
		},
	}
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedJob.EXPECT().
		ID().Return(suite.jobID)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.lostTaskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(&taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
			runtimeDiff := runtimeDiffs[suite.instanceID]
			suite.True(
				runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
			suite.True(
				runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
		}).
		Return(nil)

	suite.cachedJob.EXPECT().
		GetJobType().Return(pbjob.JobType_BATCH)

	suite.taskGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	suite.jobGoalStateEngine.EXPECT().
		Enqueue(gomock.Any(), gomock.Any()).
		Return()

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.NoError(err)
}

// TestLostTaskNoRetry tests that no retry is attempted for lost task
func (suite *TaskFailRetryTestSuite) TestLostTaskNoRetry() {
	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 0,
		},
	}
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.lostTaskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(&taskConfig, &models.ConfigAddOn{}, nil)

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.NoError(err)
}

// TestTaskFailSystemFailure test task failed becaused of system failure
func (suite *TaskFailRetryTestSuite) TestTaskFailSystemFailure() {
	suite.taskRuntime.Reason = mesosv1.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED.String()

	testTable := []*pbtask.RuntimeInfo{
		{
			MesosTaskId:   &mesosv1.TaskID{Value: &suite.mesosTaskID},
			State:         pbtask.TaskState_FAILED,
			GoalState:     pbtask.TaskState_SUCCEEDED,
			ConfigVersion: 1,
			Message:       "testFailure",
			Reason:        mesosv1.TaskStatus_REASON_CONTAINER_LAUNCH_FAILED.String(),
		},
		{
			MesosTaskId:   &mesosv1.TaskID{Value: &suite.mesosTaskID},
			State:         pbtask.TaskState_FAILED,
			GoalState:     pbtask.TaskState_SUCCEEDED,
			ConfigVersion: 1,
			Message:       "Container terminated with signal Broken pipe",
			Reason:        mesosv1.TaskStatus_REASON_COMMAND_EXECUTOR_FAILED.String(),
		},
	}

	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 0,
		},
	}

	for _, taskRuntime := range testTable {

		suite.jobFactory.EXPECT().
			GetJob(suite.jobID).Return(suite.cachedJob)

		suite.cachedJob.EXPECT().
			GetTask(suite.instanceID).Return(suite.cachedTask)

		suite.cachedJob.EXPECT().
			ID().Return(suite.jobID)

		suite.cachedTask.EXPECT().
			GetRunTime(gomock.Any()).Return(taskRuntime, nil)

		suite.taskStore.EXPECT().
			GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
			Return(&taskConfig, &models.ConfigAddOn{}, nil)

		suite.cachedJob.EXPECT().
			PatchTasks(gomock.Any(), gomock.Any()).
			Do(func(ctx context.Context, runtimeDiffs map[uint32]jobmgrcommon.RuntimeDiff) {
				runtimeDiff := runtimeDiffs[suite.instanceID]
				suite.True(
					runtimeDiff[jobmgrcommon.MesosTaskIDField].(*mesosv1.TaskID).GetValue() != suite.mesosTaskID)
				suite.True(
					runtimeDiff[jobmgrcommon.PrevMesosTaskIDField].(*mesosv1.TaskID).GetValue() == suite.mesosTaskID)
				suite.True(
					runtimeDiff[jobmgrcommon.StateField].(pbtask.TaskState) == pbtask.TaskState_INITIALIZED)
			}).
			Return(nil)

		suite.cachedJob.EXPECT().
			GetJobType().Return(pbjob.JobType_BATCH)

		suite.taskGoalStateEngine.EXPECT().
			Enqueue(gomock.Any(), gomock.Any()).
			Return()

		suite.jobGoalStateEngine.EXPECT().
			Enqueue(gomock.Any(), gomock.Any()).
			Return()

		err := TaskFailRetry(context.Background(), suite.taskEnt)
		suite.NoError(err)
	}
}

// TestTaskFailDBError tests DB failure
func (suite *TaskFailRetryTestSuite) TestTaskFailDBError() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(nil, nil, fmt.Errorf("fake db error"))

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.Error(err)

}

// TestTaskFailRetryNoJob test failed to get the cached job
func (suite *TaskFailRetryTestSuite) TestTaskFailRetryNoJob() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(nil)
	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)
}

// TestTaskFailRetryNoJob test failed to get the cached task
func (suite *TaskFailRetryTestSuite) TestTaskFailRetryNoTask() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(nil)

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.Nil(err)

}

// TestTaskFailRetryNoTask test failed to get the task runtime
func (suite *TaskFailRetryTestSuite) TestTaskFailRetryNoTaskRuntime() {
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(nil, fmt.Errorf("runtime error"))

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}

// TestTaskFailRetryFailedPatch tests retry when patch task runtime failed
func (suite *TaskFailRetryTestSuite) TestTaskFailRetryFailedPatch() {
	taskConfig := pbtask.TaskConfig{
		RestartPolicy: &pbtask.RestartPolicy{
			MaxFailures: 3,
		},
	}
	suite.jobFactory.EXPECT().
		GetJob(suite.jobID).Return(suite.cachedJob)

	suite.cachedJob.EXPECT().
		GetTask(suite.instanceID).Return(suite.cachedTask)

	suite.cachedJob.EXPECT().
		ID().Return(suite.jobID)

	suite.cachedTask.EXPECT().
		GetRunTime(gomock.Any()).Return(suite.taskRuntime, nil)

	suite.taskStore.EXPECT().
		GetTaskConfig(gomock.Any(), suite.jobID, suite.instanceID, gomock.Any()).
		Return(&taskConfig, &models.ConfigAddOn{}, nil)

	suite.cachedJob.EXPECT().
		PatchTasks(gomock.Any(), gomock.Any()).Return(fmt.Errorf("patch error"))

	err := TaskFailRetry(context.Background(), suite.taskEnt)
	suite.Error(err)
}
