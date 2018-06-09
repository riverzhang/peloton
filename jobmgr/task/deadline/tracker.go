package deadline

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/api/v0/peloton"
	pb_task "code.uber.internal/infra/peloton/.gen/peloton/api/v0/task"

	"code.uber.internal/infra/peloton/jobmgr/cached"
	"code.uber.internal/infra/peloton/jobmgr/goalstate"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/util"

	log "github.com/sirupsen/logrus"
	"github.com/uber-go/tally"
	"go.uber.org/yarpc"
)

// Config is Task deadline tracker specific config
type Config struct {
	// DeadlineTrackingPeriod is the period to check for tasks for deadline
	DeadlineTrackingPeriod time.Duration `yaml:"deadline_tracking_period"`
}

const (
	// RunningStateNotStarted represents not started state of deadline tracker component
	RunningStateNotStarted = 0
	// RunningStateRunning represents running state of deadline tracker component
	RunningStateRunning = 1
)

// Tracker defines the interface of task deadline tracker
// which tracks the tasks which are running more then
// deadline specified by the users
type Tracker interface {
	// Start starts the deadline tracker
	Start() error
	// Stop stops the deadline tracker
	Stop() error
}

// tracker implements the Tracker interface
type tracker struct {
	lock            sync.Mutex
	runningState    int32
	shutdown        int32
	jobStore        storage.JobStore
	taskStore       storage.TaskStore
	stopChan        chan struct{}
	jobFactory      cached.JobFactory
	goalStateDriver goalstate.Driver
	config          *Config
	metrics         *Metrics
}

// New creates a deadline tracker
func New(
	d *yarpc.Dispatcher,
	jobStore storage.JobStore,
	taskStore storage.TaskStore,
	jobFactory cached.JobFactory,
	goalStateDriver goalstate.Driver,
	parent tally.Scope,
	config *Config,
) Tracker {
	return &tracker{
		jobStore:        jobStore,
		taskStore:       taskStore,
		runningState:    RunningStateNotStarted,
		jobFactory:      jobFactory,
		goalStateDriver: goalStateDriver,
		config:          config,
		metrics:         NewMetrics(parent.SubScope("jobmgr").SubScope("task")),
	}
}

// Start starts Task Deadline Tracker process
func (t *tracker) Start() error {
	defer t.lock.Unlock()
	t.lock.Lock()

	t.stopChan = make(chan struct{}, 1)

	if atomic.CompareAndSwapInt32(&t.runningState, RunningStateNotStarted, RunningStateRunning) {
		go func() {
			defer atomic.StoreInt32(&t.runningState, RunningStateNotStarted)

			ticker := time.NewTicker(t.config.DeadlineTrackingPeriod)
			defer ticker.Stop()

			log.Info("Starting Deadline Tracker")

			for {
				select {
				case <-t.stopChan:
					log.Info("Exiting Deadline Tracker")
					return
				case <-ticker.C:
					err := t.trackDeadline()
					if err != nil {
						log.WithError(err).Warn("Deadline Tracker failed")
					}
				}
			}
		}()
	}
	return nil
}

// Stop stops Task Deadline Tracker process
func (t *tracker) Stop() error {
	defer t.lock.Unlock()
	t.lock.Lock()

	if t.runningState == RunningStateNotStarted {
		log.Warn("Deadline tracker is already stopped, no action will be performed")
		return nil
	}

	log.Info("Stopping Deadline tracker")

	close(t.stopChan)

	// Wait for tracker to be stopped
	for {
		runningState := atomic.LoadInt32(&t.runningState)
		if runningState == RunningStateRunning {
			time.Sleep(10 * time.Millisecond)
		} else {
			break
		}
	}
	log.Info("Deadline tracker Stopped")
	return nil
}

// trackDeadline functions keeps track of the deadline of each task
func (t *tracker) trackDeadline() error {
	jobs := t.jobFactory.GetAllJobs()

	for id, cachedJob := range jobs {
		// We need to get the job config
		ctx, cancelFunc := context.WithTimeout(context.Background(), 10*time.Second)
		cachedConfig, err := cachedJob.GetConfig(ctx)
		cancelFunc()
		if err != nil {
			log.WithField("job_id", id).
				WithError(err).
				Error("Failed to get job config")
			continue
		}

		if cachedConfig.GetSLA().GetMaxRunningTime() == uint32(0) {
			log.WithField("job_id", id).
				Info("MaximumRunningTime is 0, Not tracking this job")
			continue
		}

		for instance, info := range cachedJob.GetAllTasks() {
			runtime, err := info.GetRunTime(context.Background())
			if err != nil {
				log.WithError(err).
					WithFields(log.Fields{
						"job_id":      id,
						"instance_id": instance,
					}).Info("failed to fetch task runtime")
				continue
			}

			if runtime.GetState() != pb_task.TaskState_RUNNING {
				log.WithField("state", runtime.GetState().String()).
					Debug("Task is not running. Ignoring tracker")
				continue
			}
			st, _ := time.Parse(time.RFC3339Nano, runtime.GetStartTime())
			delta := time.Now().UTC().Sub(st)
			log.WithFields(log.Fields{
				"deadline":       cachedConfig.GetSLA().GetMaxRunningTime(),
				"time_remaining": delta,
				"job_id":         id,
				"instance":       instance,
			}).Info("Task Deadline")
			taskID := &peloton.TaskID{
				Value: fmt.Sprintf("%s-%d", id, instance),
			}
			if cachedConfig.GetSLA().GetMaxRunningTime() < uint32(delta.Seconds()) {
				log.WithField("task_id", taskID.Value).Info("Task is being killed" +
					" due to deadline exceeded")
				err := t.stopTask(context.Background(), taskID)
				if err != nil {
					log.WithField("task", taskID.Value).
						Error("task couldn't be killed " +
							"after the deadline")
					t.metrics.TaskKillFail.Inc(1)
					continue
				}
				t.metrics.TaskKillSuccess.Inc(1)
			}

		}
	}
	return nil
}

//stopTask makes the state of the task to be killed
func (t *tracker) stopTask(ctx context.Context, task *peloton.TaskID) error {

	log.WithField("task_ID", task.Value).
		Info("stopping task")

	// set goal state to TaskState_KILLED
	runtime := &pb_task.RuntimeInfo{
		GoalState: pb_task.TaskState_KILLED,
		Reason:    "Deadline exceeded",
	}

	id, instanceID, err := util.ParseTaskID(task.GetValue())
	if err != nil {
		return err
	}
	jobID := &peloton.JobID{Value: id}

	// update the task in DB and cache, and then schedule to goalstate
	cachedJob := t.jobFactory.AddJob(jobID)
	err = cachedJob.UpdateTasks(ctx, map[uint32]*pb_task.RuntimeInfo{uint32(instanceID): runtime}, cached.UpdateCacheAndDB)
	if err != nil {
		return err
	}
	t.goalStateDriver.EnqueueTask(jobID, uint32(instanceID), time.Now())
	goalstate.EnqueueJobWithDefaultDelay(jobID, t.goalStateDriver, cachedJob)
	return nil
}

func (t *tracker) isRunning() bool {
	status := atomic.LoadInt32(&t.runningState)
	return status == RunningStateRunning
}
