// +build !unit

// FIXME: Use a store mock and remove above build tag!

package respool

import (
	"container/list"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"

	"github.com/golang/mock/gomock"
	"github.com/uber-go/tally"

	"go.uber.org/yarpc"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/job"
	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/respool"
	"code.uber.internal/infra/peloton/.gen/peloton/api/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/common"

	pb_respool "code.uber.internal/infra/peloton/.gen/peloton/api/respool"

	"code.uber.internal/infra/peloton/storage/cassandra"
	store_mocks "code.uber.internal/infra/peloton/storage/mocks"
	"code.uber.internal/infra/peloton/util"
)

type resTreeTestSuite struct {
	suite.Suite
	resourceTree Tree
	mockCtrl     *gomock.Controller
	store        *cassandra.Store
	dispatcher   yarpc.Dispatcher
	resPools     map[string]*respool.ResourcePoolConfig
	allNodes     map[string]*ResPool
	root         *ResPool
	newRoot      *ResPool
}

func (suite *resTreeTestSuite) SetupSuite() {
	fmt.Println("setting up resTreeTestSuite")
	suite.mockCtrl = gomock.NewController(suite.T())
	mockResPoolStore := store_mocks.NewMockResourcePoolStore(suite.mockCtrl)
	mockResPoolStore.EXPECT().GetAllResourcePools(context.Background()).
		Return(suite.getResPools(), nil).AnyTimes()

	conf := cassandra.MigrateForTest()
	store, err := cassandra.NewStore(conf, tally.NoopScope)
	suite.NoError(err)
	suite.store = store
	suite.resourceTree = &tree{
		store:     mockResPoolStore,
		root:      nil,
		metrics:   NewMetrics(tally.NoopScope),
		resPools:  make(map[string]ResPool),
		jobStore:  suite.store,
		taskStore: suite.store,
	}
}

func (suite *resTreeTestSuite) TearDownSuite() {
	suite.mockCtrl.Finish()
}

func (suite *resTreeTestSuite) SetupTest() {
	err := suite.resourceTree.Start()
	suite.NoError(err)
}

func (suite *resTreeTestSuite) TearDownTest() {
	err := suite.resourceTree.Stop()
	suite.NoError(err)
}

// Returns resource configs
func (suite *resTreeTestSuite) getResourceConfig() []*respool.ResourceConfig {

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
			Reservation: 100,
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

// Returns resource pools
func (suite *resTreeTestSuite) getResPools() map[string]*respool.ResourcePoolConfig {

	rootID := respool.ResourcePoolID{Value: RootResPoolID}
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
			Parent:    &respool.ResourcePoolID{Value: "respool1"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool12": {
			Name:      "respool12",
			Parent:    &respool.ResourcePoolID{Value: "respool1"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool21": {
			Name:      "respool21",
			Parent:    &respool.ResourcePoolID{Value: "respool2"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool22": {
			Name:      "respool22",
			Parent:    &respool.ResourcePoolID{Value: "respool2"},
			Resources: suite.getResourceConfig(),
			Policy:    policy,
		},
		"respool23": {
			Name:   "respool23",
			Parent: &respool.ResourcePoolID{Value: "respool22"},
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
		"respool99": {
			Name:   "respool99",
			Parent: &respool.ResourcePoolID{Value: "respool21"},
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

func TestPelotonResPool(t *testing.T) {
	suite.Run(t, new(resTreeTestSuite))
}

func (suite *resTreeTestSuite) TestPrintTree() {
	// TODO: serialize the tree and compare it
	rt, ok := suite.resourceTree.(*tree)
	suite.Equal(true, ok)
	rt.printTree(rt.root)
}

func (suite *resTreeTestSuite) TestGetChildren() {
	rt, ok := suite.resourceTree.(*tree)
	suite.Equal(true, ok)
	list := rt.root.Children()
	suite.Equal(list.Len(), 3)
	n := rt.resPools["respool1"]
	list = n.Children()
	suite.Equal(list.Len(), 2)
	n = rt.resPools["respool2"]
	list = n.Children()
	suite.Equal(list.Len(), 2)
}

func (suite *resTreeTestSuite) TestResourceConfig() {
	rt, ok := suite.resourceTree.(*tree)
	suite.Equal(true, ok)
	n := rt.resPools["respool1"]
	suite.Equal(n.ID(), "respool1")
	for _, res := range n.Resources() {
		if res.Kind == "cpu" {
			assert.Equal(suite.T(), res.Reservation, 100.00, "Reservation is not Equal")
			assert.Equal(suite.T(), res.Limit, 1000.00, "Limit is not equal")
		}
		if res.Kind == "memory" {
			assert.Equal(suite.T(), res.Reservation, 100.00, "Reservation is not Equal")
			assert.Equal(suite.T(), res.Limit, 1000.00, "Limit is not equal")
		}
		if res.Kind == "disk" {
			assert.Equal(suite.T(), res.Reservation, 100.00, "Reservation is not Equal")
			assert.Equal(suite.T(), res.Limit, 1000.00, "Limit is not equal")
		}
		if res.Kind == "gpu" {
			assert.Equal(suite.T(), res.Reservation, 2.00, "Reservation is not Equal")
			assert.Equal(suite.T(), res.Limit, 4.00, "Limit is not equal")
		}
	}
}

func (suite *resTreeTestSuite) TestPendingQueueError() {
	rt, ok := suite.resourceTree.(*tree)
	suite.Equal(true, ok)
	// Task -1
	jobID1 := &peloton.JobID{
		Value: "job1",
	}
	taskID1 := &peloton.TaskID{
		Value: fmt.Sprintf("%s-%d", jobID1.Value, 1),
	}
	taskItem1 := &resmgr.Task{
		Name:     "job1-1",
		Priority: 0,
		JobId:    jobID1,
		Id:       taskID1,
	}
	err := rt.resPools["respool1"].EnqueueGang(rt.resPools["respool11"].MakeTaskGang(taskItem1))
	suite.EqualError(
		err,
		"Respool respool1 is not a leaf node",
	)
}

func (suite *resTreeTestSuite) TestPendingQueue() {
	rt, ok := suite.resourceTree.(*tree)
	suite.Equal(true, ok)
	// Task -1
	jobID1 := &peloton.JobID{
		Value: "job1",
	}
	taskID1 := &peloton.TaskID{
		Value: fmt.Sprintf("%s-%d", jobID1.Value, 1),
	}
	taskItem1 := &resmgr.Task{
		Name:     "job1-1",
		Priority: 0,
		JobId:    jobID1,
		Id:       taskID1,
		Resource: &task.ResourceConfig{
			CpuLimit:    1,
			DiskLimitMb: 10,
			GpuLimit:    0,
			MemLimitMb:  100,
		},
	}
	rt.resPools["respool11"].EnqueueGang(rt.resPools["respool11"].MakeTaskGang(taskItem1))

	// Task -2
	jobID2 := &peloton.JobID{
		Value: "job1",
	}
	taskID2 := &peloton.TaskID{
		Value: fmt.Sprintf("%s-%d", jobID2.Value, 2),
	}
	taskItem2 := &resmgr.Task{
		Name:     "job1-2",
		Priority: 0,
		JobId:    jobID2,
		Id:       taskID2,
		Resource: &task.ResourceConfig{
			CpuLimit:    1,
			DiskLimitMb: 10,
			GpuLimit:    0,
			MemLimitMb:  100,
		},
	}
	rt.resPools["respool11"].SetEntitlement(suite.getEntitlement())
	rt.resPools["respool11"].EnqueueGang(rt.resPools["respool11"].MakeTaskGang(taskItem2))

	tlist3, err := rt.resPools["respool11"].DequeueGangList(1)
	if err != nil {
		assert.Fail(suite.T(), "Dequeue should not fail")
	}
	if tlist3.Len() != 1 {
		assert.Fail(suite.T(), "Dequeue should return single task gang")
	}
	gang := tlist3.Front().Value.(*list.List)
	if gang.Len() != 1 {
		assert.Fail(suite.T(), "Dequeue single task gang should be length 1")
	}
	t1 := gang.Front().Value.(*resmgr.Task)
	assert.Equal(suite.T(), t1.JobId.Value, "job1", "Should get Job-1")
	assert.Equal(suite.T(), t1.Id.GetValue(), "job1-1", "Should get Job-1 and Task-1")

	tlist4, err2 := rt.resPools["respool11"].DequeueGangList(1)
	if err2 != nil {
		assert.Fail(suite.T(), "Dequeue should not fail")
	}
	if tlist4.Len() != 1 {
		assert.Fail(suite.T(), "Dequeue should return single task gang")
	}
	gang = tlist4.Front().Value.(*list.List)
	if gang.Len() != 1 {
		assert.Fail(suite.T(), "Dequeue single task gang should be length 1")
	}
	t2 := gang.Front().Value.(*resmgr.Task)
	assert.Equal(suite.T(), t2.JobId.Value, "job1", "Should get Job-1")
	assert.Equal(suite.T(), t2.Id.GetValue(), "job1-2", "Should get Job-1 and Task-1")
}

func (suite *resTreeTestSuite) TestTree_UpsertExistingResourcePoolConfig() {
	mockExistingResourcePoolID := &respool.ResourcePoolID{
		Value: "respool23",
	}

	mockParentPoolID := &respool.ResourcePoolID{
		Value: "respool22",
	}

	mockResourcePoolConfig := &respool.ResourcePoolConfig{
		Parent: mockParentPoolID,
		Resources: []*respool.ResourceConfig{
			{
				Reservation: 10,
				Kind:        "cpu",
				Limit:       50,
				Share:       2,
			},
		},
		Policy: respool.SchedulingPolicy_PriorityFIFO,
		Name:   mockParentPoolID.Value,
	}

	err := suite.resourceTree.Upsert(mockExistingResourcePoolID, mockResourcePoolConfig)
	suite.NoError(err)
}

func (suite *resTreeTestSuite) TestTree_UpsertNewResourcePoolConfig() {
	mockExistingResourcePoolID := &respool.ResourcePoolID{
		Value: "respool24",
	}

	mockParentPoolID := &respool.ResourcePoolID{
		Value: "respool23",
	}

	mockResourcePoolConfig := &respool.ResourcePoolConfig{
		Parent: mockParentPoolID,
		Resources: []*respool.ResourceConfig{
			{
				Reservation: 10,
				Kind:        "cpu",
				Limit:       50,
				Share:       2,
			},
		},
		Policy: respool.SchedulingPolicy_PriorityFIFO,
		Name:   mockParentPoolID.Value,
	}

	err := suite.resourceTree.Upsert(mockExistingResourcePoolID, mockResourcePoolConfig)
	suite.NoError(err)
}

func (suite *resTreeTestSuite) TestTree_UpsertNewResourcePoolConfigError() {
	mockExistingResourcePoolID := &pb_respool.ResourcePoolID{
		Value: "respool200",
	}

	mockParentPoolID := &pb_respool.ResourcePoolID{
		Value: "respool23",
	}

	mockResourcePoolConfig := &pb_respool.ResourcePoolConfig{
		Parent: mockParentPoolID,
		Resources: []*pb_respool.ResourceConfig{
			{
				Reservation: 10,
				Kind:        "cpu",
				Limit:       50,
				Share:       2,
			},
		},
		Name: mockParentPoolID.Value,
	}

	err := suite.resourceTree.Upsert(mockExistingResourcePoolID, mockResourcePoolConfig)
	suite.EqualError(
		err,
		"failed to insert resource pool: respool200: error creating resource pool respool200: Invalid queue Type",
	)
}

func (suite *resTreeTestSuite) TestTree_GetByPath() {
	// Get Root
	resPool, err := suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/",
	})
	suite.NoError(err)
	suite.Equal(resPool.Name(), "root")

	// Get respool1
	resPool, err = suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/respool1",
	})
	suite.NoError(err)
	suite.Equal(resPool.Name(), "respool1")
	suite.Equal(resPool.Parent().Name(), "root")

	// Get respool11
	resPool, err = suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/respool1/respool11",
	})
	suite.NoError(err)
	suite.Equal(resPool.Name(), "respool11")
	suite.Equal(resPool.Parent().Name(), "respool1")

	// Get non-existent pool
	resPool, err = suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/doesnotexist",
	})
	suite.Error(err)

	// Get non-existent pool
	resPool, err = suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/respool1/respool11/doesnotexist",
	})
	suite.Error(err)

	// Get on uninitialized tree fails.
	suite.resourceTree.(*tree).root = nil
	resPool, err = suite.resourceTree.GetByPath(&respool.ResourcePoolPath{
		Value: "/",
	})
	suite.Error(err)
}

func (suite *resTreeTestSuite) createJob(
	jobID *peloton.JobID,
	numTasks uint32,
	instanceCount uint32,
	minInstances uint32,
	taskState task.TaskState) {

	sla := job.SlaConfig{
		Preemptible:             false,
		Priority:                1,
		MinimumRunningInstances: minInstances,
	}
	var resPoolID respool.ResourcePoolID
	resPoolID.Value = "respool11"

	var jobConfig = job.JobConfig{
		Name:          jobID.Value,
		Sla:           &sla,
		InstanceCount: instanceCount,
		RespoolID:     &resPoolID,
	}

	var err = suite.store.CreateJob(context.Background(), jobID, &jobConfig, "uber")
	suite.NoError(err)
	for i := uint32(0); i < numTasks; i++ {
		var taskID = fmt.Sprintf("%s-%d", jobID.Value, i)
		taskConf := task.TaskConfig{
			Name: fmt.Sprintf("%s-%d", jobID.Value, i),
			Resource: &task.ResourceConfig{
				CpuLimit:   1,
				MemLimitMb: 20,
			},
		}
		var taskInfo = task.TaskInfo{
			Runtime: &task.RuntimeInfo{
				TaskId: &mesos.TaskID{Value: &taskID},
				State:  taskState,
			},
			Config:     &taskConf,
			InstanceId: i,
			JobId:      jobID,
		}
		err = suite.store.CreateTask(context.Background(), jobID, i, &taskInfo, "test")
		suite.NoError(err)
	}
}

func (suite *resTreeTestSuite) TestConvertTask() {
	ti := &task.TaskInfo{
		JobId: &peloton.JobID{
			Value: "job-1",
		},
		InstanceId: 1,
		Config:     &task.TaskConfig{},
		Runtime:    &task.RuntimeInfo{},
	}
	jobConfig := &job.JobConfig{
		Sla: &job.SlaConfig{
			Preemptible: true,
			Priority:    12,
		},
	}

	rmtask := util.ConvertTaskToResMgrTask(ti, jobConfig)
	suite.NotNil(rmtask)
	suite.EqualValues(rmtask.Priority, 12)
	suite.EqualValues(rmtask.Preemptible, true)

	ti = &task.TaskInfo{
		JobId: &peloton.JobID{
			Value: "job-2",
		},
		InstanceId: 1,
		Config:     &task.TaskConfig{},
		Runtime:    &task.RuntimeInfo{},
	}
	jobConfig = &job.JobConfig{}

	rmtask = util.ConvertTaskToResMgrTask(ti, jobConfig)
	suite.NotNil(rmtask)
	suite.EqualValues(rmtask.Priority, 0)
	suite.EqualValues(rmtask.Preemptible, false)
}

func (suite *resTreeTestSuite) getEntitlement() map[string]float64 {
	mapEntitlement := make(map[string]float64)
	mapEntitlement[common.CPU] = float64(100)
	mapEntitlement[common.MEMORY] = float64(1000)
	mapEntitlement[common.DISK] = float64(100)
	mapEntitlement[common.GPU] = float64(2)
	return mapEntitlement
}
