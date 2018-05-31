package offers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgr"
	"code.uber.internal/infra/peloton/.gen/peloton/private/resmgrsvc"
	"code.uber.internal/infra/peloton/placement/metrics"
	"code.uber.internal/infra/peloton/placement/models"

	log "github.com/sirupsen/logrus"
)

const (
	_failedToAcquireHostOffers = "failed to acquire host offers"
	_failedToFetchTasksOnHosts = "failed to fetch tasks on hosts"
	_timeout                   = 10 * time.Second
)

// Service will manage offers used by any placement strategy.
type Service interface {
	// Acquire fetches a batch of offers from the host manager.
	Acquire(ctx context.Context, fetchTasks bool, taskType resmgr.TaskType, filter *hostsvc.HostFilter) (offers []*models.Host, reason string)

	// Release returns the acquired offers back to host manager.
	Release(ctx context.Context, offers []*models.Host)
}

// NewService will create a new offer service.
func NewService(
	hostManager hostsvc.InternalHostServiceYARPCClient,
	resourceManager resmgrsvc.ResourceManagerServiceYARPCClient,
	metrics *metrics.Metrics) Service {
	return &service{
		hostManager:     hostManager,
		resourceManager: resourceManager,
		metrics:         metrics,
	}
}

type service struct {
	hostManager     hostsvc.InternalHostServiceYARPCClient
	resourceManager resmgrsvc.ResourceManagerServiceYARPCClient
	metrics         *metrics.Metrics
}

// Acquire fetches a batch of offers from the host manager.
func (s *service) Acquire(
	ctx context.Context,
	fetchTasks bool,
	taskType resmgr.TaskType,
	filter *hostsvc.HostFilter) (offers []*models.Host, reason string) {
	// Get list of host -> resources (aggregate of outstanding offers)
	hostOffers, filterResults, err := s.fetchOffers(ctx, filter)
	if err != nil {
		log.WithFields(log.Fields{
			"host_offers":    hostOffers,
			"filter_results": filterResults,
			"filter":         filter,
			"task_type":      taskType,
			"fetch_tasks":    fetchTasks,
		}).WithError(err).Error(_failedToAcquireHostOffers)
		s.metrics.OfferGetFail.Inc(1)
		return offers, _failedToAcquireHostOffers
	}

	filterRes, err := json.Marshal(filterResults)
	if err != nil {
		log.WithFields(log.Fields{
			"host_offers":         hostOffers,
			"task_type":           taskType,
			"fetch_tasks":         fetchTasks,
			"filter":              filter,
			"filter_results":      filterResults,
			"filter_results_json": string(filterRes),
		}).Error(err.Error())
		s.metrics.OfferGetFail.Inc(1)
		return offers, err.Error()
	}

	if len(hostOffers) == 0 {
		return offers, _failedToAcquireHostOffers
	}

	// Get tasks running on hosts from hostOffers
	var hostTasksMap map[string]*resmgrsvc.TaskList
	if fetchTasks && len(hostOffers) > 0 {
		hostTasksMap, err = s.fetchTasks(ctx, hostOffers, taskType)
		if err != nil {
			log.WithFields(log.Fields{
				"hostOffers":     hostOffers,
				"filter_results": filterResults,
				"filter":         filter,
				"task_type":      taskType,
				"fetch_tasks":    fetchTasks,
			}).WithError(err).Error(_failedToFetchTasksOnHosts)
			s.metrics.OfferGetFail.Inc(1)
			return offers, _failedToFetchTasksOnHosts
		}

		// Log tasks already running on Hosts whose offers are acquired.
		// TODO: (varung) - remove in long term
		log.WithFields(log.Fields{
			"host_offers":         hostOffers,
			"filter":              filter,
			"filter_results_json": string(filterRes),
			"task_type":           taskType,
			"host_task_map":       hostTasksMap,
		}).Info("Log tasks already running on Hosts whose offers are acquired")
	}

	log.WithFields(log.Fields{
		"host_offers":            hostOffers,
		"filter_results":         filterResults,
		"filter":                 filter,
		"task_type":              taskType,
		"fetch_tasks":            fetchTasks,
		"host_tasks_map_noindex": hostTasksMap,
	}).Debug("Offer service acquired offers and related tasks")

	s.metrics.OfferGet.Inc(1)

	// Create placement offers from the host offers
	return s.convertOffers(hostOffers, hostTasksMap, time.Now()), string(filterRes)
}

// Release returns the acquired offers back to host manager.
func (s *service) Release(
	ctx context.Context,
	hosts []*models.Host) {
	if len(hosts) == 0 {
		return
	}

	hostOffers := make([]*hostsvc.HostOffer, 0, len(hosts))
	for _, offer := range hosts {
		hostOffers = append(hostOffers, offer.GetOffer())
	}

	ctx, cancelFunc := context.WithTimeout(ctx, _timeout)
	defer cancelFunc()

	// ToDo: buffer the hosts until we have a batch of a certain size and return that.
	request := &hostsvc.ReleaseHostOffersRequest{
		HostOffers: hostOffers,
	}
	response, err := s.hostManager.ReleaseHostOffers(ctx, request)

	if err != nil {
		log.WithField("error", err).Error("release host offers failed")
		return
	}

	if respErr := response.GetError(); respErr != nil {
		log.WithFields(log.Fields{
			"release_host_request":        request,
			"release_host_response":       response,
			"release_host_response_error": respErr,
		}).Error("release host offers error")
		// TODO: Differentiate known error types by metrics and logs.
	} else {
		log.WithFields(log.Fields{
			"release_host_request":  request,
			"release_host_response": response,
		}).Info("release host offers request returned")
	}
}

// fetchOffers returns the offers by each host and count of all offers from host manager.
func (s *service) fetchOffers(
	ctx context.Context,
	filter *hostsvc.HostFilter) ([]*hostsvc.HostOffer, map[string]uint32, error) {
	ctx, cancelFunc := context.WithTimeout(ctx, _timeout)
	defer cancelFunc()

	offersRequest := &hostsvc.AcquireHostOffersRequest{
		Filter: filter,
	}
	offersResponse, err := s.hostManager.AcquireHostOffers(ctx, offersRequest)
	if err != nil {
		return nil, nil, err
	}

	log.WithFields(log.Fields{
		"acquire_host_offers_request":  offersRequest,
		"acquire_host_offers_response": offersResponse,
	}).Debug("acquire host offers returned")

	if respErr := offersResponse.GetError(); respErr != nil {
		return nil, nil, errors.New(respErr.String())
	}

	return offersResponse.GetHostOffers(), offersResponse.GetFilterResultCounts(), nil
}

// fetchTasks returns the tasks running on provided host from resource manager.
func (s *service) fetchTasks(
	ctx context.Context,
	hostOffers []*hostsvc.HostOffer,
	taskType resmgr.TaskType) (map[string]*resmgrsvc.TaskList, error) {
	ctx, cancelFunc := context.WithTimeout(ctx, _timeout)
	defer cancelFunc()

	// Extract the hostnames
	hostnames := make([]string, 0, len(hostOffers))
	for _, hostOffer := range hostOffers {
		hostnames = append(hostnames, hostOffer.Hostname)
	}

	// Get tasks running on provided hosts
	tasksRequest := &resmgrsvc.GetTasksByHostsRequest{
		Type:      taskType,
		Hostnames: hostnames,
	}
	tasksResponse, err := s.resourceManager.GetTasksByHosts(ctx, tasksRequest)
	if err != nil {
		return nil, err
	}

	return tasksResponse.HostTasksMap, nil
}

// convertOffers creates host offers into placement offers.
// One key notion is to add already running tasks on this host
// such that placement can take care of task-task affinity.
func (s *service) convertOffers(
	hostOffers []*hostsvc.HostOffer,
	tasks map[string]*resmgrsvc.TaskList,
	now time.Time) []*models.Host {
	offers := make([]*models.Host, 0, len(hostOffers))
	for _, hostOffer := range hostOffers {
		var taskList []*resmgr.Task
		if tasks != nil && tasks[hostOffer.Hostname] != nil {
			taskList = tasks[hostOffer.Hostname].Tasks
		}
		offers = append(offers, models.NewHost(hostOffer, taskList, now))
	}

	return offers
}
