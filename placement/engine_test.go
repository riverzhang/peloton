package placement

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"code.uber.internal/infra/peloton/.gen/mesos/v1"
	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/common/async"
	"code.uber.internal/infra/peloton/placement/config"
	"code.uber.internal/infra/peloton/placement/metrics"
	"code.uber.internal/infra/peloton/placement/models"
	offers_mock "code.uber.internal/infra/peloton/placement/offers/mocks"
	"code.uber.internal/infra/peloton/placement/plugins/batch"
	"code.uber.internal/infra/peloton/placement/plugins/mocks"
	reserver_mocks "code.uber.internal/infra/peloton/placement/reserver/mocks"
	tasks_mock "code.uber.internal/infra/peloton/placement/tasks/mocks"
	"code.uber.internal/infra/peloton/placement/testutil"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/uber-go/tally"
)

const (
	_testReason      = "Test Placement Reason"
	_portRange1Begin = 31000
	_portRange1End   = 31001
	_portRange2Begin = 31002
	_portRange2End   = 31009
)

func setupEngine(t *testing.T) (
	*gomock.Controller,
	*engine, *offers_mock.MockService,
	*tasks_mock.MockService,
	*mocks.MockStrategy) {
	ctrl := gomock.NewController(t)

	mockOfferService := offers_mock.NewMockService(ctrl)
	mockTaskService := tasks_mock.NewMockService(ctrl)
	mockStrategy := mocks.NewMockStrategy(ctrl)
	config := &config.PlacementConfig{
		TaskDequeueLimit:     10,
		OfferDequeueLimit:    10,
		MaxPlacementDuration: 30 * time.Second,
		TaskDequeueTimeOut:   100,
		TaskType:             resmgr.TaskType_BATCH,
		FetchOfferTasks:      false,
		Strategy:             config.Batch,
		Concurrency:          1,
		MaxRounds: config.MaxRoundsConfig{
			Unknown:   1,
			Batch:     1,
			Stateless: 5,
			Daemon:    5,
			Stateful:  0,
		},
		MaxDurations: config.MaxDurationsConfig{
			Unknown:   5 * time.Second,
			Batch:     5 * time.Second,
			Stateless: 10 * time.Second,
			Daemon:    15 * time.Second,
			Stateful:  25 * time.Second,
		},
	}

	e := New(
		tally.NoopScope,
		config,
		mockOfferService,
		mockTaskService,
		nil,
		mockStrategy,
		async.NewPool(async.PoolOptions{}),
	)

	return ctrl, e.(*engine), mockOfferService, mockTaskService, mockStrategy
}

func TestEnginePlaceNoTasksToPlace(t *testing.T) {
	ctrl, engine, _, mockTaskService, _ := setupEngine(t)
	defer ctrl.Finish()

	mockTaskService.EXPECT().
		Dequeue(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).
		Return(
			nil,
		)

	delay := engine.Place(context.Background())
	assert.True(t, delay > time.Duration(0))
}

// TODO: use mockgens to replace manually generated mocks.
type mockService struct {
	lockAssignments sync.Mutex
	lockHosts       sync.Mutex
	assignments     []*models.Assignment
	hosts           []*models.HostOffers
}

func (s *mockService) Acquire(
	ctx context.Context,
	fetchTasks bool,
	taskType resmgr.TaskType,
	filter *hostsvc.HostFilter) (offers []*models.HostOffers, reason string) {

	s.lockHosts.Lock()
	defer s.lockHosts.Unlock()
	result := s.hosts
	s.hosts = nil
	return result, _testReason
}

func (s *mockService) Dequeue(
	ctx context.Context,
	taskType resmgr.TaskType,
	batchSize int,
	timeout int) (assignments []*models.Assignment) {
	s.lockAssignments.Lock()
	defer s.lockAssignments.Unlock()
	result := s.assignments
	s.assignments = nil
	return result
}

func (s *mockService) Release(
	ctx context.Context,
	offers []*models.HostOffers) {
}

func (s *mockService) Enqueue(
	ctx context.Context,
	assignments []*models.Assignment,
	reason string) {
}

func (s *mockService) SetPlacements(
	ctx context.Context,
	placements []*resmgr.Placement) {
}

func TestEnginePlaceMultipleTasks(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()
	createTasks := 25
	createHosts := 10

	engine.config.MaxPlacementDuration = time.Second
	deadline := time.Now().Add(time.Second)

	var assignments []*models.Assignment
	for i := 0; i < createTasks; i++ {
		assignment := testutil.SetupAssignment(deadline, 1)
		assignment.GetTask().GetTask().Resource.CpuLimit = 5
		assignments = append(assignments, assignment)
	}

	var hosts []*models.HostOffers
	for i := 0; i < createHosts; i++ {
		hosts = append(hosts, testutil.SetupHostOffers())
	}

	mockService := &mockService{
		assignments: assignments,
		hosts:       hosts,
	}
	engine.taskService = mockService
	engine.offerService = mockService

	engine.strategy = batch.New()

	engine.Place(context.Background())
	engine.pool.WaitUntilProcessed()

	var success, failed int
	for _, assignment := range assignments {
		if assignment.GetHost() != nil {
			success++
		} else {
			failed++
		}
	}

	assert.Equal(t, createTasks, success)
	assert.Equal(t, 0, failed)
}

func TestEnginePlaceSubsetOfTasksDueToInsufficientResources(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()
	createTasks := 25
	createHosts := 10

	engine.config.MaxPlacementDuration = time.Second
	deadline := time.Now().Add(time.Second)
	var assignments []*models.Assignment
	for i := 0; i < createTasks; i++ {
		assignment := testutil.SetupAssignment(deadline, 1)
		assignments = append(assignments, assignment)
	}
	var hosts []*models.HostOffers
	for i := 0; i < createHosts; i++ {
		hosts = append(hosts, testutil.SetupHostOffers())
	}
	mockService := &mockService{
		assignments: assignments,
		hosts:       hosts,
	}
	engine.taskService = mockService
	engine.offerService = mockService

	engine.strategy = batch.New()

	engine.Place(context.Background())
	engine.pool.WaitUntilProcessed()

	var success, failed int
	for _, assignment := range assignments {
		if assignment.GetHost() != nil {
			success++
		} else {
			failed++
		}
	}
	assert.Equal(t, 10, success)
	assert.Equal(t, 15, failed)
}

// Test tasks cannot get placed due to no host offer.
func TestEnginePlaceNoHostsMakesTaskExceedDeadline(t *testing.T) {
	ctrl, engine, mockOfferService, mockTaskService, _ := setupEngine(t)
	defer ctrl.Finish()
	engine.config.MaxPlacementDuration = time.Millisecond
	assignment := testutil.SetupAssignment(time.Now().Add(time.Millisecond), 1)
	assignments := []*models.Assignment{assignment}

	mockOfferService.EXPECT().
		Acquire(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).MinTimes(1).
		Return(nil, _testReason)

	mockTaskService.EXPECT().
		Enqueue(
			gomock.Any(),
			gomock.Any(),
			_testReason,
		).Times(1).
		Return()

	filter := &hostsvc.HostFilter{}
	engine.placeAssignmentGroup(context.Background(), filter, assignments)
}

func TestEnginePlaceTaskExceedMaxRoundsAndGetsPlaced(t *testing.T) {
	ctrl, engine, mockOfferService, mockTaskService, mockStrategy := setupEngine(t)
	defer ctrl.Finish()
	engine.config.MaxPlacementDuration = 1 * time.Second

	host := testutil.SetupHostOffers()
	offers := []*models.HostOffers{host}
	assignment := testutil.SetupAssignment(time.Now().Add(1*time.Second), 5)
	assignment.SetHost(host)
	assignments := []*models.Assignment{assignment}

	mockStrategy.EXPECT().
		PlaceOnce(
			gomock.Any(),
			gomock.Any(),
		).
		Times(5).
		Return()

	mockTaskService.EXPECT().
		SetPlacements(
			gomock.Any(),
			gomock.Any(),
		).MinTimes(1).
		Return()

	mockOfferService.EXPECT().
		Acquire(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).MinTimes(1).
		Return(offers, _testReason)

	filter := &hostsvc.HostFilter{}
	engine.placeAssignmentGroup(context.Background(), filter, assignments)
}

func TestEnginePlaceCallToStrategy(t *testing.T) {
	ctrl, engine, mockOfferService, mockTaskService, mockStrategy := setupEngine(t)
	defer ctrl.Finish()
	engine.config.MaxPlacementDuration = 100 * time.Millisecond

	host := testutil.SetupHostOffers()
	hosts := []*models.HostOffers{host}
	assignment := testutil.SetupAssignment(time.Now(), 1)
	assignment.SetHost(host)
	assignments := []*models.Assignment{assignment}

	mockTaskService.EXPECT().
		Dequeue(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).
		Return(
			assignments,
		)

	mockOfferService.EXPECT().
		Acquire(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).MinTimes(1).
		Return(
			hosts,
			_testReason,
		)

	mockStrategy.EXPECT().
		PlaceOnce(
			gomock.Any(),
			gomock.Any(),
		).
		Return()

	mockStrategy.EXPECT().
		Filters(
			gomock.Any(),
		).MinTimes(1).
		Return(map[*hostsvc.HostFilter][]*models.Assignment{nil: assignments})

	mockStrategy.EXPECT().
		ConcurrencySafe().
		AnyTimes().
		Return(false)

	mockTaskService.EXPECT().
		Enqueue(
			gomock.Any(),
			gomock.Any(),
			gomock.Any(),
		).AnyTimes().
		Return()

	mockTaskService.EXPECT().
		SetPlacements(
			gomock.Any(),
			gomock.Any(),
		).AnyTimes().
		Return()

	mockOfferService.EXPECT().
		Release(
			gomock.Any(),
			gomock.Any(),
		).AnyTimes().
		Return()

	delay := engine.Place(context.Background())
	assert.Equal(t, time.Duration(0), delay)
}

func TestEngineFindUsedOffers(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	now := time.Now()
	deadline := now.Add(30 * time.Second)
	assignment := testutil.SetupAssignment(deadline, 1)
	assignments := []*models.Assignment{
		assignment,
	}
	used := engine.findUsedHosts(assignments)
	assert.Equal(t, 0, len(used))

	host := testutil.SetupHostOffers()
	assignment.SetHost(host)
	used = engine.findUsedHosts(assignments)
	assert.Equal(t, 1, len(used))
	assert.Equal(t, host, used[0])
}

func TestEngineFilterAssignments(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	deadline1 := time.Now()
	now := deadline1.Add(1 * time.Second)
	deadline2 := now.Add(30 * time.Second)
	host := testutil.SetupHostOffers()

	assignment1 := testutil.SetupAssignment(deadline1, 1) // assigned
	assignment1.SetHost(host)

	assignment2 := testutil.SetupAssignment(deadline2, 2) // retryable
	assignment2.SetHost(host)

	assignment3 := testutil.SetupAssignment(deadline2, 1) // retryable

	assignment4 := testutil.SetupAssignment(deadline1, 1) // unassigned

	assignments := []*models.Assignment{
		assignment1,
		assignment2,
		assignment3,
		assignment4,
	}

	assigned, retryable, unassigned := engine.filterAssignments(now, assignments)
	assert.Equal(t, 1, len(assigned))
	assert.Equal(t, []*models.Assignment{assignment1}, assigned)
	assert.Equal(t, 2, len(retryable))
	assert.Equal(t, []*models.Assignment{assignment2, assignment3}, retryable)
	assert.Equal(t, 1, len(unassigned))
	assert.Equal(t, []*models.Assignment{assignment4}, unassigned)
}

func TestEngineCleanup(t *testing.T) {
	ctrl, engine, _, mockTaskService, _ := setupEngine(t)
	defer ctrl.Finish()

	host := testutil.SetupHostOffers()
	hosts := []*models.HostOffers{host}
	assignment := testutil.SetupAssignment(time.Now(), 1)
	assignment.SetHost(host)
	assignments := []*models.Assignment{assignment}

	mockTaskService.EXPECT().
		SetPlacements(
			gomock.Any(),
			gomock.Any(),
		).
		Return()

	mockTaskService.EXPECT().
		Enqueue(
			gomock.Any(),
			assignments,
			_failedToPlaceTaskAfterTimeout,
		).
		Return()

	engine.cleanup(context.Background(), assignments, nil, assignments, hosts)
}

func TestEngineCreatePlacement(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	now := time.Now()
	deadline := now.Add(30 * time.Second)
	host := testutil.SetupHostOffers()
	assignment1 := testutil.SetupAssignment(deadline, 1)
	assignment1.SetHost(host)
	assignment2 := testutil.SetupAssignment(deadline, 1)
	assignments := []*models.Assignment{
		assignment1,
		assignment2,
	}

	placements := engine.createPlacement(assignments)
	assert.Equal(t, 1, len(placements))
	assert.Equal(t, host.GetOffer().Hostname, placements[0].Hostname)
	assert.Equal(t, host.GetOffer().AgentId, placements[0].AgentId)
	assert.Equal(t, assignment1.GetTask().GetTask().Type, placements[0].Type)
	assert.Equal(t, []*peloton.TaskID{assignment1.GetTask().GetTask().Id}, placements[0].Tasks)
	assert.Equal(t, 3, len(placements[0].Ports))
}

func TestEngineAssignPortsAllFromASingleRange(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	now := time.Now()
	deadline := now.Add(30 * time.Second)
	assignment1 := testutil.SetupAssignment(deadline, 1)
	assignment2 := testutil.SetupAssignment(deadline, 1)
	tasks := []*models.Task{
		assignment1.GetTask(),
		assignment2.GetTask(),
	}
	offer := testutil.SetupHostOffers()

	ports := engine.assignPorts(offer, tasks)
	assert.Equal(t, 6, len(ports))
	assert.Equal(t, uint32(31000), ports[0])
	assert.Equal(t, uint32(31001), ports[1])
	assert.Equal(t, uint32(31002), ports[2])
	assert.Equal(t, uint32(31003), ports[3])
	assert.Equal(t, uint32(31004), ports[4])
	assert.Equal(t, uint32(31005), ports[5])
}

func TestEngineAssignPortsFromMultipleRanges(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	now := time.Now()
	deadline := now.Add(30 * time.Second)
	assignment1 := testutil.SetupAssignment(deadline, 1)
	assignment2 := testutil.SetupAssignment(deadline, 1)
	tasks := []*models.Task{
		assignment1.GetTask(),
		assignment2.GetTask(),
	}
	host := testutil.SetupHostOffers()
	*host.GetOffer().Resources[4].Ranges.Range[0].End = uint64(_portRange1End)
	begin, end := uint64(_portRange2Begin), uint64(_portRange2End)
	host.GetOffer().Resources[4].Ranges.Range = append(
		host.GetOffer().Resources[4].Ranges.Range, &mesos_v1.Value_Range{
			Begin: &begin,
			End:   &end,
		})

	ports := engine.assignPorts(host, tasks)
	intPorts := make([]int, len(ports))
	for i := range ports {
		intPorts[i] = int(ports[i])
	}
	sort.Ints(intPorts)
	assert.Equal(t, 6, len(ports))

	// range 1 (31000-31001) and range 2 (31002-31009)
	// ports selected from both ranges
	if intPorts[0] == _portRange1Begin {
		for i := 0; i < len(intPorts); i++ {
			assert.Equal(t, intPorts[i], 31000+i)
		}
	} else {
		// ports selected from range2 only
		for i := 0; i < len(intPorts); i++ {
			assert.Equal(t, intPorts[i], _portRange2Begin+i)
		}
	}
}

func TestEngineFindUnusedOffers(t *testing.T) {
	ctrl, engine, _, _, _ := setupEngine(t)
	defer ctrl.Finish()

	deadline := time.Now().Add(30 * time.Second)
	assignment1 := testutil.SetupAssignment(deadline, 1)
	assignment2 := testutil.SetupAssignment(deadline, 1)
	assignment3 := testutil.SetupAssignment(deadline, 1)
	assignment4 := testutil.SetupAssignment(deadline, 1)
	assignments := []*models.Assignment{
		assignment1,
		assignment2,
	}
	retryable := []*models.Assignment{
		assignment3,
		assignment4,
	}
	host1 := testutil.SetupHostOffers()
	host2 := testutil.SetupHostOffers()
	assignment1.SetHost(host1)
	assignment2.SetHost(host1)
	assignment3.SetHost(host1)
	offers := []*models.HostOffers{
		host1,
		host2,
	}

	unused := engine.findUnusedHosts(assignments, retryable, offers)
	assert.Equal(t, 1, len(unused))
	assert.Equal(t, host2, unused[0])
}

// TestProcessCompletedReservations tests the process completed reservations
// functions.
func TestProcessCompletedReservations(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	metrics := metrics.NewMetrics(tally.NoopScope)
	mockOfferService := offers_mock.NewMockService(ctrl)
	mockTaskService := tasks_mock.NewMockService(ctrl)
	mockStrategy := mocks.NewMockStrategy(ctrl)
	mockReserver := reserver_mocks.NewMockReserver(ctrl)
	config := &config.PlacementConfig{
		Strategy: config.Batch,
	}
	engine := &engine{
		config:       config,
		metrics:      metrics,
		offerService: mockOfferService,
		taskService:  mockTaskService,
		strategy:     mockStrategy,
		reserver:     mockReserver,
	}
	// Testing the scenario where GetCompletetedReservation returns error
	mockReserver.EXPECT().GetCompletetedReservation(gomock.Any()).Return(nil, errors.New("error"))
	err := engine.processCompletedReservations(context.Background())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error")

	// Testing the scenario where GetCompletetedReservation returns with no reservations
	var reservations []*hostsvc.CompletedReservation
	mockReserver.EXPECT().GetCompletetedReservation(gomock.Any()).Return(reservations, nil)
	err = engine.processCompletedReservations(context.Background())
	assert.Contains(t, err.Error(), "no valid reservations")

	// Testing the scenario where GetCompletetedReservation returns with valid reservations
	host := "host"
	reservations = append(reservations, &hostsvc.CompletedReservation{
		HostOffers: []*hostsvc.HostOffer{
			{
				Hostname: host,
				AgentId: &mesos_v1.AgentID{
					Value: &host,
				},
			},
		},
		Task: &resmgr.Task{
			Id: &peloton.TaskID{
				Value: "task1",
			},
		},
	})
	mockReserver.EXPECT().GetCompletetedReservation(gomock.Any()).Return(reservations, nil)
	mockTaskService.EXPECT().SetPlacements(gomock.Any(), gomock.Any())
	err = engine.processCompletedReservations(context.Background())
	assert.NoError(t, err)
}
