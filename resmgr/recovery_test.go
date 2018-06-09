package resmgr

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/respool"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"

	"code.uber.internal/infra/peloton/common"
	"code.uber.internal/infra/peloton/common/eventstream"
	"code.uber.internal/infra/peloton/common/queue"
	rc "code.uber.internal/infra/peloton/resmgr/common"
	rp "code.uber.internal/infra/peloton/resmgr/respool"
	rm_task "code.uber.internal/infra/peloton/resmgr/task"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"
)

type recoveryTestSuite struct {
	suite.Suite
	resourceTree     rp.Tree
	rmTaskTracker    rm_task.Tracker
	handler          *ServiceHandler
	taskScheduler    rm_task.Scheduler
	recovery         recoveryHandler
	mockCtrl         *gomock.Controller
	mockResPoolStore *store_mocks.MockResourcePoolStore
	mockJobStore     *store_mocks.MockJobStore
	mockTaskStore    *store_mocks.MockTaskStore
}

func (suite *recoveryTestSuite) SetupSuite() {
	suite.mockCtrl = gomock.NewController(suite.T())
	suite.mockResPoolStore = store_mocks.NewMockResourcePoolStore(suite.mockCtrl)
	suite.mockResPoolStore.EXPECT().GetAllResourcePools(context.Background()).
		Return(suite.getResPools(), nil).AnyTimes()
	suite.mockJobStore = store_mocks.NewMockJobStore(suite.mockCtrl)
	suite.mockTaskStore = store_mocks.NewMockTaskStore(suite.mockCtrl)

	rp.InitTree(tally.NoopScope, suite.mockResPoolStore, suite.mockJobStore,
		suite.mockTaskStore,
		rc.PreemptionConfig{
			Enabled: false,
		})
	suite.resourceTree = rp.GetTree()
	// Initializing the resmgr state machine
	rm_task.InitTaskTracker(tally.NoopScope, &rm_task.Config{
		EnablePlacementBackoff: true,
	})
	suite.rmTaskTracker = rm_task.GetTracker()
	rm_task.InitScheduler(tally.NoopScope, 100*time.Second, suite.rmTaskTracker)
	suite.taskScheduler = rm_task.GetScheduler()

	suite.handler = &ServiceHandler{
		metrics:     NewMetrics(tally.NoopScope),
		resPoolTree: suite.resourceTree,
		placements: queue.NewQueue(
			"placement-queue",
			reflect.TypeOf(&resmgr.Placement{}),
			maxPlacementQueueSize,
		),
		rmTracker: suite.rmTaskTracker,
		config: Config{
			RmTaskConfig: &rm_task.Config{
				LaunchingTimeout: 1 * time.Minute,
				PlacingTimeout:   1 * time.Minute,
				PolicyName:       rm_task.ExponentialBackOffPolicy,
			},
		},
	}
	suite.handler.eventStreamHandler = eventstream.NewEventStreamHandler(
		1000,
		[]string{
			common.PelotonJobManager,
			common.PelotonResourceManager,
		},
		nil,
		tally.Scope(tally.NoopScope))
}

func (suite *recoveryTestSuite) SetupTest() {
	err := suite.resourceTree.Start()
	suite.NoError(err)

	err = suite.taskScheduler.Start()
	suite.NoError(err)
}

// Returns resource pools
func (suite *recoveryTestSuite) getResPools() map[string]*respool.ResourcePoolConfig {
	rootID := peloton.ResourcePoolID{Value: common.RootResPoolID}
	policy := respool.SchedulingPolicy_PriorityFIFO

	return map[string]*respool.ResourcePoolConfig{
		// NB: root resource pool node is not stored in the database
		"respool1": {
			Name:      "respool1",
			Parent:    &rootID,
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool2": {
			Name:      "respool2",
			Parent:    &rootID,
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool3": {
			Name:      "respool3",
			Parent:    &rootID,
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool11": {
			Name:      "respool11",
			Parent:    &peloton.ResourcePoolID{Value: "respool1"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool12": {
			Name:      "respool12",
			Parent:    &peloton.ResourcePoolID{Value: "respool1"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool21": {
			Name:      "respool21",
			Parent:    &peloton.ResourcePoolID{Value: "respool2"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool22": {
			Name:      "respool22",
			Parent:    &peloton.ResourcePoolID{Value: "respool2"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool23": {
			Name:   "respool23",
			Parent: &peloton.ResourcePoolID{Value: "respool22"},
			Resources: []*respool.ResourceConfig{
				{
					Kind:        "cpu",
					Reservation: 50,
					Limit:       100,
					Share:       1,
				},
			},
			Policy: policy,
		},
	}
}

// Returns resource configs
func (suite *recoveryTestSuite) getResourceConfig() []*respool.ResourceConfig {

	resConfigs := []*respool.ResourceConfig{
		{
			Share:       1,
			Kind:        "cpu",
			Reservation: 100,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "memory",
			Reservation: 1000,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "disk",
			Reservation: 100,
			Limit:       1000,
		},
		{
			Share:       1,
			Kind:        "gpu",
			Reservation: 2,
			Limit:       4,
		},
	}
	return resConfigs
}

func (suite *recoveryTestSuite) TearDownSuite() {
	suite.resourceTree.Stop()
	suite.rmTaskTracker.Clear()
	suite.mockCtrl.Finish()
}

func (suite *recoveryTestSuite) getEntitlement() map[string]float64 {
	mapEntitlement := make(map[string]float64)
	mapEntitlement[common.CPU] = float64(100)
	mapEntitlement[common.MEMORY] = float64(1000)
	mapEntitlement[common.DISK] = float64(100)
	mapEntitlement[common.GPU] = float64(2)
	return mapEntitlement
}

// returns a map of JobID -> array of gangs
func (suite *recoveryTestSuite) getQueueContent(
	respoolID peloton.ResourcePoolID) map[string][]*resmgrsvc.Gang {

	var result = make(map[string][]*resmgrsvc.Gang)
	for {
		node, err := suite.resourceTree.Get(&respoolID)
		suite.NoError(err)
		node.SetEntitlement(suite.getEntitlement())
		dequeuedGangs, err := node.DequeueGangs(1)
		suite.NoError(err)

		if len(dequeuedGangs) == 0 {
			break
		}
		gang := dequeuedGangs[0]
		if gang != nil {
			jobID := gang.Tasks[0].JobId.Value
			_, ok := result[jobID]
			if !ok {
				var gang []*resmgrsvc.Gang
				result[jobID] = gang
			}
			result[jobID] = append(result[jobID], gang)
		}
	}
	return result
}

func (suite *recoveryTestSuite) createJob(jobID *peloton.JobID, instanceCount uint32, minInstances uint32) *job.JobConfig {
	return &job.JobConfig{
		Name: jobID.Value,
		Sla: &job.SlaConfig{
			Preemptible:             true,
			Priority:                1,
			MinimumRunningInstances: minInstances,
		},
		InstanceCount: instanceCount,
		RespoolID: &peloton.ResourcePoolID{
			Value: "respool21",
		},
	}
}

func (suite *recoveryTestSuite) createTasks(jobID *peloton.JobID, numTasks uint32, taskState task.TaskState) map[uint32]*task.TaskInfo {
	tasks := map[uint32]*task.TaskInfo{}
	var i uint32
	for i = uint32(0); i < numTasks; i++ {
		var taskID = fmt.Sprintf("%s-%d", jobID.Value, i)
		taskConf := task.TaskConfig{
			Name: fmt.Sprintf("%s-%d", jobID.Value, i),
			Resource: &task.ResourceConfig{
				CpuLimit:   1,
				MemLimitMb: 20,
			},
		}
		tasks[i] = &task.TaskInfo{
			Runtime: &task.RuntimeInfo{
				MesosTaskId:   &mesos.TaskID{Value: &taskID},
				State:         taskState,
				ConfigVersion: uint64(0),
			},
			Config:     &taskConf,
			InstanceId: i,
			JobId:      jobID,
		}
	}

	// Add 1 tasks in SUCCESS state
	var taskID = fmt.Sprintf("%s-%d", jobID.Value, i+1)
	taskConf := task.TaskConfig{
		Name: fmt.Sprintf("%s-%d", jobID.Value, i+1),
		Resource: &task.ResourceConfig{
			CpuLimit:   1,
			MemLimitMb: 20,
		},
	}
	tasks[i+1] = &task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			MesosTaskId: &mesos.TaskID{Value: &taskID},
			State:       task.TaskState_SUCCEEDED,
		},
		Config:     &taskConf,
		InstanceId: i,
		JobId:      jobID,
	}
	return tasks
}

func (suite *recoveryTestSuite) TestRefillTaskQueue() {
	// Create jobs. each with different number of tasks
	jobs := make([]peloton.JobID, 4)
	for i := 0; i < 4; i++ {
		jobs[i] = peloton.JobID{Value: fmt.Sprintf("TestJob_%d", i)}
	}

	// create 4 jobs ( 2 JobState_RUNNING, 2 JobState_PENDING)
	fmt.Println(suite.mockJobStore, suite.mockCtrl)
	suite.mockJobStore.EXPECT().GetJobsByStates(context.Background(), gomock.Eq([]job.JobState{
		job.JobState_INITIALIZED,
		job.JobState_PENDING,
		job.JobState_RUNNING,
		job.JobState_UNKNOWN,
	})).Return(jobs, nil)

	suite.mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), &jobs[0]).
		Return(&job.RuntimeInfo{
			State:     job.JobState_RUNNING,
			GoalState: job.JobState_SUCCEEDED,
		}, nil)

	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), &jobs[0]).
		Return(suite.createJob(&jobs[0], 10, 1), nil)
	suite.mockTaskStore.EXPECT().
		GetTasksForJobByRange(context.Background(), &jobs[0], &task.InstanceRange{
			From: 0,
			To:   10,
		}).
		Return(suite.createTasks(&jobs[0], 9, task.TaskState_INITIALIZED), nil)

	suite.mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), &jobs[1]).
		Return(&job.RuntimeInfo{
			State:     job.JobState_RUNNING,
			GoalState: job.JobState_SUCCEEDED,
		}, nil)

	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), &jobs[1]).
		Return(suite.createJob(&jobs[1], 10, 10), nil)
	suite.mockTaskStore.EXPECT().
		GetTasksForJobByRange(context.Background(), &jobs[1], &task.InstanceRange{
			From: 0,
			To:   10,
		}).
		Return(suite.createTasks(&jobs[1], 9, task.TaskState_PENDING), nil)

	suite.mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), &jobs[2]).
		Return(&job.RuntimeInfo{
			State:     job.JobState_RUNNING,
			GoalState: job.JobState_SUCCEEDED,
		}, nil)

	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), &jobs[2]).
		Return(suite.createJob(&jobs[2], 10, 1), nil)
	suite.mockTaskStore.EXPECT().
		GetTasksForJobByRange(context.Background(), &jobs[2], &task.InstanceRange{
			From: 0,
			To:   10,
		}).
		Return(suite.createTasks(&jobs[2], 9, task.TaskState_RUNNING), nil)

	suite.mockJobStore.EXPECT().
		GetJobRuntime(context.Background(), &jobs[3]).
		Return(&job.RuntimeInfo{
			State:     job.JobState_RUNNING,
			GoalState: job.JobState_SUCCEEDED,
		}, nil)

	suite.mockJobStore.EXPECT().
		GetJobConfig(context.Background(), &jobs[3]).
		Return(suite.createJob(&jobs[3], 10, 1), nil)
	suite.mockTaskStore.EXPECT().
		GetTasksForJobByRange(context.Background(), &jobs[3], &task.InstanceRange{
			From: 0,
			To:   10,
		}).
		Return(suite.createTasks(&jobs[3], 9, task.TaskState_LAUNCHED), nil)

	// Perform recovery
	InitRecovery(tally.NoopScope, suite.mockJobStore, suite.mockTaskStore, suite.handler,
		Config{
			RmTaskConfig: &rm_task.Config{
				LaunchingTimeout: 1 * time.Minute,
				PlacingTimeout:   1 * time.Minute,
				PolicyName:       rm_task.ExponentialBackOffPolicy,
			},
		})

	rh := GetRecoveryHandler().(*recoveryHandler)
	rh.Start()
	<-rh.finished

	// 2. check the queue content
	var resPoolID peloton.ResourcePoolID
	resPoolID.Value = "respool21"
	gangsSummary := suite.getQueueContent(resPoolID)

	suite.Equal(1, len(gangsSummary))

	// Job0 should not be recovered
	gangs := gangsSummary["TestJob_0"]
	suite.Equal(0, len(gangs))

	// Job1 should be recovered and should have 1 gang(9 tasks per gang) in the pending queue
	gangs = gangsSummary["TestJob_1"]
	suite.Equal(1, len(gangs))
	for _, gang := range gangs {
		suite.Equal(9, len(gang.GetTasks()))
	}

	// Checking total number of tasks in the tracker(9*3=27)
	// This includes tasks from TestJob_2 which are not re queued but are inserted in the tracker
	suite.Equal(suite.rmTaskTracker.GetSize(), int64(27))
}

func TestResmgrRecovery(t *testing.T) {
	suite.Run(t, new(recoveryTestSuite))
}
