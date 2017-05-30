package job

import (
	"context"
	"fmt"
	"testing"
	"time"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	res_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc/mocks"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/storage/cassandra"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"
	"code.uber.internal/infra/peloton/util"
	log "github.com/Sirupsen/logrus"
	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"github.com/uber-go/tally"
)

var csStore *cassandra.Store

func init() {
	conf := cassandra.MigrateForTest()
	var err error
	csStore, err = cassandra.NewStore(conf, tally.NoopScope)
	if err != nil {
		log.Fatal(err)
	}
}

func TestValidatorWithStore(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockClient := res_mocks.NewMockResourceManagerServiceYarpcClient(ctrl)

	var sentTasks = make(map[int]bool)
	mockClient.EXPECT().EnqueueGangs(
		gomock.Any(),
		gomock.Any()).
		Do(func(_ context.Context, reqBody interface{}) {
			req := reqBody.(*resmgrsvc.EnqueueGangsRequest)
			for _, gang := range req.GetGangs() {
				for _, task := range gang.GetTasks() {
					_, instance, err := util.ParseTaskID(task.Id.Value)
					assert.Nil(t, err)
					sentTasks[instance] = true
				}
			}
		}).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil).AnyTimes()

	var jobID = &peloton.JobID{Value: "TestValidatorWithStore"}
	var sla = job.SlaConfig{
		Priority:                22,
		MaximumRunningInstances: 3,
		Preemptible:             false,
	}
	var taskConfig = task.TaskConfig{
		Resource: &task.ResourceConfig{
			CpuLimit:    0.8,
			MemLimitMb:  800,
			DiskLimitMb: 1500,
		},
	}
	var jobConfig = job.JobConfig{
		Name:          "TestValidatorWithStore",
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "gsg9"},
		Sla:           &sla,
		DefaultConfig: &taskConfig,
		InstanceCount: 10,
	}
	err := csStore.CreateJob(context.Background(), jobID, &jobConfig, "gsg9")
	assert.Nil(t, err)

	// Create a job with instanceCount 10 but only 5 tasks
	// validate that after recovery, all tasks are created and
	// the job state is update to pending.
	jobRuntime, err := csStore.GetJobRuntime(context.Background(), jobID)
	assert.Nil(t, err)
	jobRuntime.CreationTime = (time.Now().Add(-10 * time.Hour)).String()

	for i := uint32(0); i < uint32(3); i++ {
		_, err := createTaskForJob(
			context.Background(),
			csStore,
			jobID,
			i,
			&jobConfig)
		assert.Nil(t, err)
	}

	for i := uint32(7); i < uint32(9); i++ {
		_, err := createTaskForJob(
			context.Background(),
			csStore,
			jobID,
			i,
			&jobConfig)
		assert.Nil(t, err)
	}
	err = csStore.UpdateJobRuntime(context.Background(), jobID, jobRuntime)
	assert.Nil(t, err)

	validator := NewJobRecovery(csStore, csStore, mockClient, tally.NoopScope)
	validator.recoverJob(context.Background(), jobID)

	jobRuntime, err = csStore.GetJobRuntime(context.Background(), jobID)
	assert.Nil(t, err)
	assert.Equal(t, job.JobState_PENDING, jobRuntime.State)
	for i := uint32(0); i < jobConfig.InstanceCount; i++ {
		_, err := csStore.GetTaskForJob(context.Background(), jobID, i)
		assert.Nil(t, err)
		assert.True(t, sentTasks[int(i)])
	}
}

func TestValidator(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	jobID := &peloton.JobID{
		Value: "job0",
	}
	jobConfig := job.JobConfig{
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "qa"},
		InstanceCount: uint32(3),
		Sla:           &job.SlaConfig{},
	}
	var jobRuntime = job.RuntimeInfo{
		State:        job.JobState_INITIALIZED,
		CreationTime: (time.Now().Add(-10 * time.Hour)).String(),
	}
	var tasks = []*task.TaskInfo{
		createTaskInfo(jobID, uint32(1), task.TaskState_INITIALIZED),
		createTaskInfo(jobID, uint32(2), task.TaskState_INITIALIZED),
	}
	var createdTasks []*task.TaskInfo
	var sentTasks = make(map[int]bool)
	var mockJobStore = store_mocks.NewMockJobStore(ctrl)
	var mockTaskStore = store_mocks.NewMockTaskStore(ctrl)
	mockClient := res_mocks.NewMockResourceManagerServiceYarpcClient(ctrl)

	mockJobStore.EXPECT().
		GetJobsByState(context.Background(), job.JobState_INITIALIZED).
		Return([]peloton.JobID{*jobID}, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		GetJobConfig(context.Background(), gomock.Any()).
		Return(&jobConfig, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), gomock.Any()).
		Return(&jobRuntime, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		UpdateJobRuntime(context.Background(), jobID, gomock.Any()).
		Return(nil).
		AnyTimes()

	mockTaskStore.EXPECT().
		CreateTask(context.Background(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, id *peloton.JobID, instanceID uint32, taskInfo *task.TaskInfo, _ string) {
			createdTasks = append(createdTasks, taskInfo)
		}).Return(nil)
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(1)).
		Return(nil, &storage.TaskNotFoundError{TaskID: ""})
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(0)).
		Return(map[uint32]*task.TaskInfo{
			uint32(0): createTaskInfo(jobID, uint32(0), task.TaskState_RUNNING),
		}, nil)
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(2)).
		Return(map[uint32]*task.TaskInfo{
			uint32(2): tasks[1],
		}, nil)
	mockClient.EXPECT().EnqueueGangs(
		gomock.Any(),
		gomock.Any()).
		Do(func(_ context.Context, reqBody interface{}) {
			req := reqBody.(*resmgrsvc.EnqueueGangsRequest)
			for _, gang := range req.GetGangs() {
				for _, task := range gang.GetTasks() {
					_, instance, err := util.ParseTaskID(task.Id.Value)
					assert.Nil(t, err)
					sentTasks[instance] = true
				}
			}
		}).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil)

	validator := NewJobRecovery(mockJobStore, mockTaskStore, mockClient, tally.NoopScope)
	validator.recoverJobs(context.Background())

	// jobRuntime create time is long ago, thus after validateJobs(),
	// job runtime state should be pending and task 1 and task 2 are sent by resmgr client
	assert.Equal(t, job.JobState_PENDING, jobRuntime.State)
	assert.True(t, sentTasks[1])
	assert.True(t, sentTasks[2])

	// jobRuntime create time is recent, thus validateJobs() should skip the validation
	jobRuntime.State = job.JobState_INITIALIZED
	jobRuntime.CreationTime = time.Now().String()
	sentTasks = make(map[int]bool)

	validator.recoverJobs(context.Background())

	assert.Equal(t, job.JobState_INITIALIZED, jobRuntime.State)
	assert.False(t, sentTasks[1])
	assert.False(t, sentTasks[2])
}

func TestValidatorFailures(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	jobID := &peloton.JobID{
		Value: "job0",
	}
	jobConfig := job.JobConfig{
		OwningTeam:    "team6",
		LdapGroups:    []string{"money", "team6", "qa"},
		InstanceCount: uint32(3),
		Sla:           &job.SlaConfig{},
	}
	var jobRuntime = job.RuntimeInfo{
		State:        job.JobState_INITIALIZED,
		CreationTime: (time.Now().Add(-10 * time.Hour)).String(),
	}
	var tasks = []*task.TaskInfo{
		createTaskInfo(jobID, uint32(1), task.TaskState_INITIALIZED),
		createTaskInfo(jobID, uint32(2), task.TaskState_INITIALIZED),
	}
	var createdTasks []*task.TaskInfo
	var sentTasks = make(map[int]bool)
	var mockJobStore = store_mocks.NewMockJobStore(ctrl)
	var mockTaskStore = store_mocks.NewMockTaskStore(ctrl)
	mockClient := res_mocks.NewMockResourceManagerServiceYarpcClient(ctrl)

	mockJobStore.EXPECT().
		GetJobsByState(context.Background(), job.JobState_INITIALIZED).
		Return([]peloton.JobID{*jobID}, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		GetJobConfig(context.Background(), gomock.Any()).
		Return(&jobConfig, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), gomock.Any()).
		Return(&jobRuntime, nil).
		AnyTimes()
	mockJobStore.EXPECT().
		UpdateJobRuntime(context.Background(), jobID, gomock.Any()).
		Return(errors.New("Mock error")).
		MinTimes(1).
		MaxTimes(1)
	mockJobStore.EXPECT().
		UpdateJobRuntime(context.Background(), jobID, gomock.Any()).
		Return(nil).
		AnyTimes()

	mockTaskStore.EXPECT().
		CreateTask(context.Background(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Do(func(ctx context.Context, id *peloton.JobID, instanceID uint32, taskInfo *task.TaskInfo, _ string) {
			createdTasks = append(createdTasks, taskInfo)
		}).Return(nil).
		AnyTimes()
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(1)).
		Return(nil, &storage.TaskNotFoundError{TaskID: ""}).
		AnyTimes()
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(0)).
		Return(map[uint32]*task.TaskInfo{
			uint32(0): createTaskInfo(jobID, uint32(0), task.TaskState_RUNNING),
		}, nil).
		AnyTimes()
	mockTaskStore.EXPECT().
		GetTaskForJob(context.Background(), gomock.Any(), uint32(2)).
		Return(map[uint32]*task.TaskInfo{
			uint32(2): tasks[1],
		}, nil).
		AnyTimes()
	mockClient.EXPECT().EnqueueGangs(
		gomock.Any(),
		gomock.Any()).
		Return(nil, errors.New("Mock client error")).
		MinTimes(1).
		MaxTimes(1)
	mockClient.EXPECT().EnqueueGangs(
		gomock.Any(),
		gomock.Any()).
		Do(func(_ context.Context, reqBody interface{}) {
			req := reqBody.(*resmgrsvc.EnqueueGangsRequest)
			for _, gang := range req.GetGangs() {
				for _, task := range gang.GetTasks() {
					_, instance, err := util.ParseTaskID(task.Id.Value)
					assert.Nil(t, err)
					sentTasks[instance] = true
				}
			}
		}).
		Return(&resmgrsvc.EnqueueGangsResponse{}, nil).
		AnyTimes()

	validator := NewJobRecovery(mockJobStore, mockTaskStore, mockClient, tally.NoopScope)
	validator.recoverJobs(context.Background())

	// First call would fail as the client would fail once, nothing changed to the job
	assert.Equal(t, job.JobState_INITIALIZED, jobRuntime.State)
	assert.False(t, sentTasks[1])
	assert.False(t, sentTasks[2])

	// Second call would fail as the jobstore updateJobRuntime would fail once,
	// nothing changed to the job but tasks sent
	validator = NewJobRecovery(mockJobStore, mockTaskStore, mockClient, tally.NoopScope)
	validator.recoverJobs(context.Background())
	assert.Equal(t, job.JobState_PENDING, jobRuntime.State)
	assert.True(t, sentTasks[1])
	assert.True(t, sentTasks[2])

	// jobRuntime create time is long ago, thus after validateJobs(),
	// job runtime state should be pending and task 1 and task 2 are sent by resmgr client
	validator = NewJobRecovery(mockJobStore, mockTaskStore, mockClient, tally.NoopScope)
	validator.recoverJobs(context.Background())
	assert.Equal(t, job.JobState_PENDING, jobRuntime.State)
	assert.True(t, sentTasks[1])
	assert.True(t, sentTasks[2])
}

func createTaskInfo(jobID *peloton.JobID, i uint32, state task.TaskState) *task.TaskInfo {
	var tID = fmt.Sprintf("%s-%d-%s", jobID.Value, i, uuid.NewUUID().String())
	var taskInfo = task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			TaskId: &mesos.TaskID{Value: &tID},
			State:  state,
		},
		Config: &task.TaskConfig{
			Name: tID,
		},
		InstanceId: uint32(i),
		JobId:      jobID,
	}
	return &taskInfo
}
