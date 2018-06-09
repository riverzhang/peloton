package task

import (
	"context"
	"errors"
	"fmt"
	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/assert"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	host_mocks "code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc/mocks"
	"github.com/stretchr/testify/suite"
)

type KillOrphanTaskTestSuite struct {
	suite.Suite
	ctrl        *gomock.Controller
	ctx         context.Context
	mockHostMgr *host_mocks.MockInternalHostServiceYARPCClient
	jobID       string
	instanceID  int32
	mesosTaskID string
	taskInfo    task.TaskInfo
}

func (suite *KillOrphanTaskTestSuite) SetupTest() {
	suite.ctrl = gomock.NewController(suite.T())
	suite.ctx = context.Background()
	suite.mockHostMgr = host_mocks.NewMockInternalHostServiceYARPCClient(suite.ctrl)
	suite.jobID = "af647b98-0ae0-4dac-be42-c74a524dfe44"
	suite.instanceID = 89
	suite.mesosTaskID = fmt.Sprintf(
		"%s-%d-%s",
		suite.jobID,
		suite.instanceID,
		uuid.New())
	suite.taskInfo = task.TaskInfo{
		Runtime: &task.RuntimeInfo{
			MesosTaskId: &mesos.TaskID{Value: &suite.mesosTaskID},
			AgentID:     &mesos.AgentID{Value: &suite.mesosTaskID},
			State:       task.TaskState_RUNNING,
		},
		Config:     &task.TaskConfig{},
		InstanceId: uint32(0),
		JobId:      &peloton.JobID{Value: suite.jobID},
	}
}

// TestNormalKill tests the happy case of KillOrphanTask
func (suite *KillOrphanTaskTestSuite) TestNormalKill() {
	suite.mockHostMgr.EXPECT().
		KillTasks(gomock.Any(), &hostsvc.KillTasksRequest{
			TaskIds: []*mesos.TaskID{{Value: &suite.mesosTaskID}},
		})

	err := KillOrphanTask(suite.ctx, suite.mockHostMgr, &suite.taskInfo)
	assert.Nil(suite.T(), err)
}

// TestErrorKill tests when hostmgt returns error
func (suite *KillOrphanTaskTestSuite) TestErrorKill() {
	suite.mockHostMgr.EXPECT().
		KillTasks(gomock.Any(), &hostsvc.KillTasksRequest{
			TaskIds: []*mesos.TaskID{{Value: &suite.mesosTaskID}},
		}).Return(nil, errors.New(""))

	err := KillOrphanTask(suite.ctx, suite.mockHostMgr, &suite.taskInfo)
	assert.NotNil(suite.T(), err)
}

// TestKillKilling tests when kill an orphan task in KILLING state
func (suite *KillOrphanTaskTestSuite) TestKillKilling() {
	suite.taskInfo.Runtime.State = task.TaskState_KILLING
	suite.mockHostMgr.EXPECT().ShutdownExecutors(gomock.Any(), gomock.Any())
	err := KillOrphanTask(suite.ctx, suite.mockHostMgr, &suite.taskInfo)
	assert.NotNil(suite.T(), err)
}

// TestKillKilling tests when kill an orphan task has no Mesos ID
func (suite *KillOrphanTaskTestSuite) TestKillNoMesosID() {
	suite.taskInfo.Runtime = &task.RuntimeInfo{
		State: task.TaskState_RUNNING,
	}
	suite.mockHostMgr.EXPECT()
	err := KillOrphanTask(suite.ctx, suite.mockHostMgr, &suite.taskInfo)
	assert.NotNil(suite.T(), err)
}

// TestKillKilling tests when kill a stateful orphan
func (suite *KillOrphanTaskTestSuite) TestKillStateful() {
	suite.taskInfo.Config = &task.TaskConfig{
		Volume: &task.PersistentVolumeConfig{
			ContainerPath: "/A/B/C",
			SizeMB:        1024,
		},
	}
	suite.taskInfo.Runtime = &task.RuntimeInfo{
		MesosTaskId: &mesos.TaskID{Value: &suite.mesosTaskID},
		State:       task.TaskState_RUNNING,
		VolumeID:    &peloton.VolumeID{Value: "peloton_id"},
	}
	suite.mockHostMgr.EXPECT()
	err := KillOrphanTask(suite.ctx, suite.mockHostMgr, &suite.taskInfo)
	assert.NotNil(suite.T(), err)
}
