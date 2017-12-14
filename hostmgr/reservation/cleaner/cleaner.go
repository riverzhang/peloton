package cleaner

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/uber-go/atomic"
	"github.com/uber-go/tally"

	mesos "code.uber.internal/infra/peloton/.gen/mesos/v1"
	sched "code.uber.internal/infra/peloton/.gen/mesos/v1/scheduler"
	"code.uber.internal/infra/peloton/.gen/peloton/private/hostmgr/hostsvc"

	"code.uber.internal/infra/peloton/.gen/peloton/api/peloton"
	"code.uber.internal/infra/peloton/.gen/peloton/api/volume"
	"code.uber.internal/infra/peloton/hostmgr/factory/operation"
	hostmgrmesos "code.uber.internal/infra/peloton/hostmgr/mesos"
	"code.uber.internal/infra/peloton/hostmgr/offer/offerpool"
	"code.uber.internal/infra/peloton/hostmgr/reservation"
	"code.uber.internal/infra/peloton/storage"
	"code.uber.internal/infra/peloton/yarpc/encoding/mpb"
)

const (
	_defaultContextTimeout = 10 * time.Second
)

// Cleaner is the interface to recycle the to be deleted volumes and reserved resources.
type Cleaner interface {
	Run(isRunning *atomic.Bool)
}

// cleaner implements interface Cleaner to recycle reserved resources.
type cleaner struct {
	offerPool                  offerpool.Pool
	scope                      tally.Scope
	volumeStore                storage.PersistentVolumeStore
	mSchedulerClient           mpb.SchedulerClient
	mesosFrameworkInfoProvider hostmgrmesos.FrameworkInfoProvider
}

// NewCleaner initializes the reservation resource cleaner.
func NewCleaner(
	pool offerpool.Pool,
	scope tally.Scope,
	volumeStore storage.PersistentVolumeStore,
	schedulerClient mpb.SchedulerClient,
	frameworkInfoProvider hostmgrmesos.FrameworkInfoProvider) Cleaner {

	return &cleaner{
		offerPool:                  pool,
		scope:                      scope,
		volumeStore:                volumeStore,
		mSchedulerClient:           schedulerClient,
		mesosFrameworkInfoProvider: frameworkInfoProvider,
	}
}

// Run will clean the unused reservation resources and volumes.
func (c *cleaner) Run(isRunning *atomic.Bool) {
	log.Info("Cleaning reserved resources and volumes")
	c.cleanUnusedVolumes()
	log.Info("Cleaning reserved resources and volumes returned")
}

func (c *cleaner) cleanUnusedVolumes() {
	reservedOffers := c.offerPool.GetReservedOffers()
	for hostname, offerMap := range reservedOffers {
		for _, offer := range offerMap {
			if err := c.cleanOffer(offer); err != nil {
				log.WithError(err).WithFields(log.Fields{
					"hostname": hostname,
					"offer":    offer,
				}).Error("failed to clean offer")
				continue
			}
		}
	}
}

func (c *cleaner) cleanReservedResources(offer *mesos.Offer, reservationLabel string) error {
	log.WithFields(log.Fields{
		"offer": offer,
		"label": reservationLabel,
	}).Info("Cleaning reserved resources")

	// Remove given offer from memory before destroy/unreserve.
	c.offerPool.RemoveReservedOffer(offer.GetHostname(), offer.GetId().GetValue())

	operations := []*hostsvc.OfferOperation{
		{
			Type: hostsvc.OfferOperation_UNRESERVE,
			Unreserve: &hostsvc.OfferOperation_Unreserve{
				Label: reservationLabel,
			},
		},
	}
	return c.callMesosForOfferOperations(offer, operations)
}

func (c *cleaner) cleanVolume(offer *mesos.Offer, volumeID string, reservationLabel string) error {
	log.WithFields(log.Fields{
		"offer":     offer,
		"volume_id": volumeID,
		"label":     reservationLabel,
	}).Info("Cleaning volume resources")

	// Remove given offer from memory before destroy/unreserve.
	c.offerPool.RemoveReservedOffer(offer.GetHostname(), offer.GetId().GetValue())

	operations := []*hostsvc.OfferOperation{
		{
			Type: hostsvc.OfferOperation_DESTROY,
			Destroy: &hostsvc.OfferOperation_Destroy{
				VolumeID: volumeID,
			},
		},
		{
			Type: hostsvc.OfferOperation_UNRESERVE,
			Unreserve: &hostsvc.OfferOperation_Unreserve{
				Label: reservationLabel,
			},
		},
	}
	return c.callMesosForOfferOperations(offer, operations)
}

// cleanOffer calls mesos master to destroy/unreserve resources that needs to be cleaned.
func (c *cleaner) cleanOffer(offer *mesos.Offer) error {
	reservedResources := reservation.GetLabeledReservedResources([]*mesos.Offer{offer})

	for labels, res := range reservedResources {
		if len(res.Volumes) == 0 {
			return c.cleanReservedResources(offer, labels)
		} else if c.needCleanVolume(res.Volumes[0], offer) {
			return c.cleanVolume(offer, res.Volumes[0], labels)
		}
	}
	return nil
}

func (c *cleaner) needCleanVolume(volumeID string, offer *mesos.Offer) bool {
	ctx, cancel := context.WithTimeout(context.Background(), _defaultContextTimeout)
	defer cancel()

	volumeInfo, err := c.volumeStore.GetPersistentVolume(ctx, &peloton.VolumeID{
		Value: volumeID,
	})
	if err != nil {
		// Do not clean volume if db read error.
		log.WithError(err).WithFields(log.Fields{
			"volume_id": volumeID,
			"offer":     offer,
		}).Error("Failed to read db for given volume")
		return false
	}

	if volumeInfo.GetGoalState() == volume.VolumeState_DELETED {
		return true
	}

	return false
}

func (c *cleaner) callMesosForOfferOperations(
	offer *mesos.Offer,
	hostOperations []*hostsvc.OfferOperation) error {

	ctx, cancel := context.WithTimeout(context.Background(), _defaultContextTimeout)
	defer cancel()

	factory := operation.NewOfferOperationsFactory(
		hostOperations,
		offer.GetResources(),
		offer.GetHostname(),
		offer.GetAgentId(),
	)
	offerOperations, err := factory.GetOfferOperations()
	if err != nil {
		return err
	}

	callType := sched.Call_ACCEPT
	msg := &sched.Call{
		FrameworkId: c.mesosFrameworkInfoProvider.GetFrameworkID(ctx),
		Type:        &callType,
		Accept: &sched.Call_Accept{
			OfferIds:   []*mesos.OfferID{offer.GetId()},
			Operations: offerOperations,
		},
	}

	log.WithFields(log.Fields{
		"offer": offer,
		"call":  msg,
	}).Info("cleaning offer with operations")

	msid := c.mesosFrameworkInfoProvider.GetMesosStreamID(ctx)
	return c.mSchedulerClient.Call(msid, msg)
}