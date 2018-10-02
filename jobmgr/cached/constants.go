package cached

import (
	"time"

	pbtask "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"

	"go.uber.org/yarpc/yarpcerrors"
)

// UpdateRequest is used to indicate whether the caller wants to update only
// cache or update both database and cache. This is used during job manager recovery
// as only cache needs to be updated during recovery.
type UpdateRequest int

const (
	// _defaultMaxParallelBatches indicates how many maximum parallel go routines will
	// be run to create/update task runtimes of a job
	_defaultMaxParallelBatches = 1000

	// time duration at which cache metrics are computed
	_defaultMetricsUpdateTick = 5 * time.Minute

	// UpdateCacheOnly updates only the cache. It should be used only during
	// recovery. Also, it requires passing the complete runtime information.
	UpdateCacheOnly UpdateRequest = iota + 1
	// UpdateCacheAndDB updates both DB and cache. The caller can pass the
	// complete runtime info or just a diff.
	UpdateCacheAndDB
)

var (
	// jobMgrOwnedTaskStates are task states to which a task is transitioned
	// within the job manager and not from an eventstream.
	jobMgrOwnedTaskStates = map[pbtask.TaskState]bool{
		pbtask.TaskState_UNKNOWN:     true,
		pbtask.TaskState_INITIALIZED: true,
		pbtask.TaskState_LAUNCHED:    true,
		pbtask.TaskState_KILLING:     true,
	}

	// resMgrOwnedTaskStates are task states which indicate that the
	// task is either waiting for admission or being placed or being preempted.
	resMgrOwnedTaskStates = map[pbtask.TaskState]bool{
		pbtask.TaskState_PENDING:    true,
		pbtask.TaskState_READY:      true,
		pbtask.TaskState_PLACING:    true,
		pbtask.TaskState_PLACED:     true,
		pbtask.TaskState_LAUNCHING:  true,
		pbtask.TaskState_PREEMPTING: true,
	}

	// mesosOwnedTaskStates are task states to which a task is transitioned through
	// an event in the event stream from mesos.
	mesosOwnedTaskStates = map[pbtask.TaskState]bool{
		pbtask.TaskState_STARTING:  true,
		pbtask.TaskState_RUNNING:   true,
		pbtask.TaskState_SUCCEEDED: true,
		pbtask.TaskState_FAILED:    true,
		pbtask.TaskState_LOST:      true,
		pbtask.TaskState_KILLED:    true,
	}
)

// InstanceIDExceedsInstanceCountError is the error returned when an operation
// for an instance with instanceID larger than job instance count is requested
var InstanceIDExceedsInstanceCountError = yarpcerrors.InvalidArgumentErrorf(
	"instance-id is beyond the instance count")
