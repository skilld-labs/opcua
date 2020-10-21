package opcua

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/skilld-labs/opcua/debug"
	"github.com/skilld-labs/opcua/errors"
	"github.com/skilld-labs/opcua/id"
	"github.com/skilld-labs/opcua/ua"
	"github.com/skilld-labs/opcua/uasc"
)

const (
	DefaultSubscriptionMaxNotificationsPerPublish = 10000
	DefaultSubscriptionLifetimeCount              = 10000
	DefaultSubscriptionMaxKeepAliveCount          = 3000
	DefaultSubscriptionInterval                   = 100 * time.Millisecond
	DefaultSubscriptionPriority                   = 0
)

const terminatedSubscriptionID uint32 = 0xC0CAC01B

type Subscription struct {
	SubscriptionID            uint32
	RevisedPublishingInterval time.Duration
	RevisedLifetimeCount      uint32
	RevisedMaxKeepAliveCount  uint32
	Notifs                    chan *PublishNotificationData
	params                    *SubscriptionParameters
	items                     []*monitoredItem
	lastSequenceNumber        uint32
	pausech                   chan struct{}
	resumech                  chan struct{}
	stopch                    chan struct{}
	c                         *Client
}

type SubscriptionParameters struct {
	Interval                   time.Duration
	LifetimeCount              uint32
	MaxKeepAliveCount          uint32
	MaxNotificationsPerPublish uint32
	Priority                   uint8
}

type monitoredItem struct {
	ItemToMonitor        *ua.ReadValueID
	MonitoringParameters *ua.MonitoringParameters
	MonitoringMode       ua.MonitoringMode
	TimestampsToReturn   ua.TimestampsToReturn
	createResult         *ua.MonitoredItemCreateResult
}

func NewMonitoredItemCreateRequestWithDefaults(nodeID *ua.NodeID, attributeID ua.AttributeID, clientHandle uint32) *ua.MonitoredItemCreateRequest {
	if attributeID == 0 {
		attributeID = ua.AttributeIDValue
	}
	return &ua.MonitoredItemCreateRequest{
		ItemToMonitor: &ua.ReadValueID{
			NodeID:       nodeID,
			AttributeID:  attributeID,
			DataEncoding: &ua.QualifiedName{},
		},
		MonitoringMode: ua.MonitoringModeReporting,
		RequestedParameters: &ua.MonitoringParameters{
			ClientHandle:     clientHandle,
			DiscardOldest:    true,
			Filter:           nil,
			QueueSize:        10,
			SamplingInterval: 0.0,
		},
	}
}

type PublishNotificationData struct {
	SubscriptionID uint32
	Error          error
	Value          interface{}
}

// Cancel stops the subscription and removes it
// from the client and the server.
func (s *Subscription) Cancel() error {
	s.c.forgetSubscription(s.SubscriptionID)
	close(s.stopch)
	return s.delete()
}

// delete removes the subscription from the server.
func (s *Subscription) delete() error {
	req := &ua.DeleteSubscriptionsRequest{
		SubscriptionIDs: []uint32{s.SubscriptionID},
	}
	var res *ua.DeleteSubscriptionsResponse
	err := s.c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	switch {
	case err != nil:
		return err
	case res.ResponseHeader.ServiceResult != ua.StatusOK:
		return res.ResponseHeader.ServiceResult
	default:
		return nil
	}
}

func (s *Subscription) Monitor(ts ua.TimestampsToReturn, items ...*ua.MonitoredItemCreateRequest) (*ua.CreateMonitoredItemsResponse, error) {
	// Part 4, 5.12.2.2 CreateMonitoredItems Service Parameters
	req := &ua.CreateMonitoredItemsRequest{
		SubscriptionID:     s.SubscriptionID,
		TimestampsToReturn: ts,
		ItemsToCreate:      items,
	}

	var res *ua.CreateMonitoredItemsResponse
	err := s.c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})

	if err != nil {
		return nil, err
	}

	for _, result := range res.Results {
		if status := result.StatusCode; status != ua.StatusOK {
			return nil, status
		}
	}

	// store monitored items
	for i, item := range items {
		result := res.Results[i]

		mi := &monitoredItem{
			ItemToMonitor:        item.ItemToMonitor,
			MonitoringParameters: item.RequestedParameters,
			MonitoringMode:       item.MonitoringMode,
			TimestampsToReturn:   ts,
			createResult:         result,
		}
		s.items = append(s.items, mi)
	}

	return res, err
}

func (s *Subscription) Unmonitor(monitoredItemIDs ...uint32) (*ua.DeleteMonitoredItemsResponse, error) {
	req := &ua.DeleteMonitoredItemsRequest{
		MonitoredItemIDs: monitoredItemIDs,
		SubscriptionID:   s.SubscriptionID,
	}
	var res *ua.DeleteMonitoredItemsResponse
	err := s.c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// SetTriggering sends a request to the server to add and/or remove triggering links from a triggering item.
// To add links from a triggering item to an item to report provide the server assigned ID(s) in the `add` argument.
// To remove links from a triggering item to an item to report provide the server assigned ID(s) in the `remove` argument.
func (s *Subscription) SetTriggering(triggeringItemID uint32, add, remove []uint32) (*ua.SetTriggeringResponse, error) {
	// Part 4, 5.12.5.2 SetTriggering Service Parameters
	req := &ua.SetTriggeringRequest{
		SubscriptionID:   s.SubscriptionID,
		TriggeringItemID: triggeringItemID,
		LinksToAdd:       add,
		LinksToRemove:    remove,
	}

	var res *ua.SetTriggeringResponse
	err := s.c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

// republish executes a synchronous republish request.
func (s *Subscription) republish(req *ua.RepublishRequest) (*ua.RepublishResponse, error) {
	var res *ua.RepublishResponse
	err := s.c.sechan.SendRequest(req, s.c.Session().resp.AuthenticationToken, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

func (s *Subscription) publish(acks []*ua.SubscriptionAcknowledgement) (*ua.PublishResponse, error) {
	if acks == nil {
		acks = []*ua.SubscriptionAcknowledgement{}
	}
	req := &ua.PublishRequest{
		SubscriptionAcknowledgements: acks,
	}

	var res *ua.PublishResponse
	err := s.c.sendWithTimeout(req, s.publishTimeout(), func(v interface{}) error {
		return safeAssign(v, &res)
	})
	return res, err
}

func (s *Subscription) publishTimeout() time.Duration {
	timeout := time.Duration(s.RevisedMaxKeepAliveCount) * s.RevisedPublishingInterval // expected keepalive interval
	if timeout > uasc.MaxTimeout {
		return uasc.MaxTimeout
	}
	if timeout < s.c.cfg.RequestTimeout {
		return s.c.cfg.RequestTimeout
	}
	return timeout
}

// pause suspends the run loop by signalling the pausech.
// Since the channel is unbuffered we wait until the
// run loop has completed the current publish message.
func (s *Subscription) pause(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.stopch:
	case s.pausech <- struct{}{}:
	}
}

// resume restarts the run loop by signalling the resumech.
// It has no effect if the run loop isn't paused.
func (s *Subscription) resume(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-s.stopch:
	case s.resumech <- struct{}{}:
	}
}

// Run starts an infinite loop which sends PublishRequests and delivers received
// notifications to registered subcribers.
//
// It is the responsibility of the caller to stop the run loops by
// cancelling the context.
//
// Note that Run may return before the context is cancelled
// in case of an irrecoverable communication error.
func (s *Subscription) Run(ctx context.Context) {
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer log.Print("sub: done")

publish:
	for {
		log.Print("sub: select")
		select {
		case <-ctx.Done():
			log.Println("sub: ctx.Done()")
			cancel()
			return

		case <-s.stopch:
			log.Println("sub: stop")
			cancel()
			return

		case <-s.pausech:
			log.Print("sub: pause")
		paused:
			for {
				select {
				case <-ctx.Done():
					log.Print("sub: pause: ctx.Done()")
					cancel()
					return
				case <-s.stopch:
					log.Print("sub: pause: stop")
					cancel()
					return
				case <-s.pausech:
					log.Print("sub: pause: pause")
					// ignore since already paused
					continue paused
				case <-s.resumech:
					log.Print("sub: pause: resume")
					continue publish
				}
			}

		case <-s.resumech:
			log.Print("sub: resume")
			// ignore since not paused
			continue

		default:
			log.Print("sub: publish")
			s.run(cctx)
		}
	}
}

func (s *Subscription) run(ctx context.Context) {
	defer log.Print("publish: done")

	var acks []*ua.SubscriptionAcknowledgement

	for {
		log.Print("publish: select")
		select {
		case <-ctx.Done():
			log.Print("publish: ctx.Done()")
			return

		case <-s.stopch:
			log.Printf("publish: stop")
			return

		default:
			log.Printf("publish: default")
			// send the next publish request
			// note that res contains data even if an error was returned
			res, err := s.publish(acks)
			switch {
			case err == ua.StatusBadSequenceNumberUnknown:
				// At least one ack has been submitted repeatedly
				// Ignore the error. Acks will be cleared below
			case err == ua.StatusBadTimeout:
				// ignore and continue the loop
			case err == ua.StatusBadNoSubscription:
				// All subscriptions have been deleted, but the publishing loop is still running
				// The user will stop the loop or create subscriptions at his discretion
			case err != nil:
				// irrecoverable error
				s.c.notifySubscriptionsOfError(ctx, res, err)
				debug.Printf("subscription %v Run loop stopped", s.SubscriptionID)
				log.Print("publish: notify error")
				return
			}

			if res != nil {
				// Prepare SubscriptionAcknowledgement for next PublishRequest
				acks = make([]*ua.SubscriptionAcknowledgement, 0)
				if res.AvailableSequenceNumbers != nil {
					for _, i := range res.AvailableSequenceNumbers {
						ack := &ua.SubscriptionAcknowledgement{
							SubscriptionID: res.SubscriptionID,
							SequenceNumber: i,
						}
						if i > atomic.LoadUint32(&s.lastSequenceNumber) {
							atomic.StoreUint32(&s.lastSequenceNumber, i)
						}
						acks = append(acks, ack)
					}
				}
			}

			if err == nil {
				s.c.notifySubscription(ctx, res)
				log.Print("publish: notify")
			}
		}
	}
}

func (s *Subscription) notify(ctx context.Context, data *PublishNotificationData) {
	select {
	case <-ctx.Done():
		return
	case s.Notifs <- data:
	}
}

// Stats returns a diagnostic struct with metadata about the current subscription
func (s *Subscription) Stats() (*ua.SubscriptionDiagnosticsDataType, error) {
	// TODO(kung-foo): once browsing feature is merged, attempt to get direct access to the
	// diagnostics node. for example, Prosys lists them like:
	// i=2290/ns=1;g=918ee6f4-2d25-4506-980d-e659441c166d
	// maybe cache the nodeid to speed up future stats queries
	node := s.c.Node(ua.NewNumericNodeID(0, id.Server_ServerDiagnostics_SubscriptionDiagnosticsArray))
	v, err := node.Value()
	if err != nil {
		return nil, err
	}

	for _, eo := range v.Value().([]*ua.ExtensionObject) {
		stat := eo.Value.(*ua.SubscriptionDiagnosticsDataType)

		if stat.SubscriptionID == s.SubscriptionID {
			return stat, nil
		}
	}

	return nil, errors.Errorf("unable to find SubscriptionDiagnostics for sub=%d", s.SubscriptionID)
}

func (p *SubscriptionParameters) setDefaults() {
	if p.MaxNotificationsPerPublish == 0 {
		p.MaxNotificationsPerPublish = DefaultSubscriptionMaxNotificationsPerPublish
	}
	if p.LifetimeCount == 0 {
		p.LifetimeCount = DefaultSubscriptionLifetimeCount
	}
	if p.MaxKeepAliveCount == 0 {
		p.MaxKeepAliveCount = DefaultSubscriptionMaxKeepAliveCount
	}
	if p.Interval == 0 {
		p.Interval = DefaultSubscriptionInterval
	}
	if p.Priority == 0 {
		// DefaultSubscriptionPriority is 0 at the time of writing, so this redundant assignment is
		// made only to allow for a one-liner change of default priority should a need arise
		// and to explicitly expose the default priority as a constant
		p.Priority = DefaultSubscriptionPriority
	}
}

// restore creates a new subscription based on the previous subscription
// parameters and monitored items.
func (s *Subscription) restore() error {
	if s.SubscriptionID == terminatedSubscriptionID {
		debug.Printf("Subscription is not in a valid state")
		return nil
	}

	params := s.params
	{
		req := &ua.DeleteSubscriptionsRequest{
			SubscriptionIDs: []uint32{s.SubscriptionID},
		}
		var res *ua.DeleteSubscriptionsResponse
		_ = s.c.Send(req, func(v interface{}) error {
			return safeAssign(v, &res)
		})
	}
	s.c.forgetSubscription(s.SubscriptionID)

	req := &ua.CreateSubscriptionRequest{
		RequestedPublishingInterval: float64(params.Interval / time.Millisecond),
		RequestedLifetimeCount:      params.LifetimeCount,
		RequestedMaxKeepAliveCount:  params.MaxKeepAliveCount,
		PublishingEnabled:           true,
		MaxNotificationsPerPublish:  params.MaxNotificationsPerPublish,
		Priority:                    params.Priority,
	}
	var res *ua.CreateSubscriptionResponse
	err := s.c.Send(req, func(v interface{}) error {
		return safeAssign(v, &res)
	})
	if err != nil {
		return err
	}
	// todo (unknownet): check if necessary
	if status := res.ResponseHeader.ServiceResult; status != ua.StatusOK {
		return status
	}

	s.SubscriptionID = res.SubscriptionID
	s.RevisedPublishingInterval = time.Duration(res.RevisedPublishingInterval) * time.Millisecond
	s.RevisedLifetimeCount = res.RevisedLifetimeCount
	s.RevisedMaxKeepAliveCount = res.RevisedMaxKeepAliveCount
	atomic.StoreUint32(&s.lastSequenceNumber, 0)

	if err := s.c.registerSubscription(s); err != nil {
		return err
	}

	// Sort by timestamp to return
	itemsByTs := make(map[ua.TimestampsToReturn][]*ua.MonitoredItemCreateRequest)
	for _, m := range s.items {
		cr := &ua.MonitoredItemCreateRequest{
			ItemToMonitor:       m.ItemToMonitor,
			MonitoringMode:      m.MonitoringMode,
			RequestedParameters: m.MonitoringParameters,
		}
		itemsByTs[m.TimestampsToReturn] = append(itemsByTs[m.TimestampsToReturn], cr)
	}

	for ts, items := range itemsByTs {
		req := &ua.CreateMonitoredItemsRequest{
			SubscriptionID:     s.SubscriptionID,
			TimestampsToReturn: ts,
			ItemsToCreate:      items,
		}

		var res *ua.CreateMonitoredItemsResponse
		err := s.c.Send(req, func(v interface{}) error {
			return safeAssign(v, &res)
		})
		if err != nil {
			return err
		}
		for _, result := range res.Results {
			if status := result.StatusCode; status != ua.StatusOK {
				return status
			}
		}

		for i, m := range s.items {
			m.createResult = res.Results[i]
		}
	}

	return nil
}
