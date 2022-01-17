// Copyright 2017 Vector Creations Ltd
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	eduserverAPI "github.com/matrix-org/dendrite/eduserver/api"
	federationAPI "github.com/matrix-org/dendrite/federationapi/api"
	"github.com/matrix-org/dendrite/internal"
	keyapi "github.com/matrix-org/dendrite/keyserver/api"
	"github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/setup/config"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.uber.org/atomic"
)

const (
	// Event was passed to the roomserver
	MetricsOutcomeOK = "ok"
	// Event failed to be processed
	MetricsOutcomeFail = "fail"
	// Event failed auth checks
	MetricsOutcomeRejected = "rejected"
	// Terminated the transaction
	MetricsOutcomeFatal = "fatal"
	// The event has missing auth_events we need to fetch
	MetricsWorkMissingAuthEvents = "missing_auth_events"
	// No work had to be done as we had all prev/auth events
	MetricsWorkDirect = "direct"
	// The event has missing prev_events we need to call /g_m_e for
	MetricsWorkMissingPrevEvents = "missing_prev_events"
)

var (
	pduCountTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "dendrite",
			Subsystem: "federationapi",
			Name:      "recv_pdus",
			Help:      "Number of incoming PDUs from remote servers with labels for success",
		},
		[]string{"status"}, // 'success' or 'total'
	)
	eduCountTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "dendrite",
			Subsystem: "federationapi",
			Name:      "recv_edus",
			Help:      "Number of incoming EDUs from remote servers",
		},
	)
	processEventSummary = prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Namespace: "dendrite",
			Subsystem: "federationapi",
			Name:      "process_event",
			Help:      "How long it takes to process an incoming event and what work had to be done for it",
		},
		[]string{"work", "outcome"},
	)
)

func init() {
	prometheus.MustRegister(
		pduCountTotal, eduCountTotal, processEventSummary,
	)
}

type sendFIFOQueue struct {
	tasks  []*inputTask
	count  int
	mutex  sync.Mutex
	notifs chan struct{}
}

func newSendFIFOQueue() *sendFIFOQueue {
	q := &sendFIFOQueue{
		notifs: make(chan struct{}, 1),
	}
	return q
}

func (q *sendFIFOQueue) push(frame *inputTask) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	q.tasks = append(q.tasks, frame)
	q.count++
	select {
	case q.notifs <- struct{}{}:
	default:
	}
}

// pop returns the first item of the queue, if there is one.
// The second return value will indicate if a task was returned.
func (q *sendFIFOQueue) pop() (*inputTask, bool) {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.count == 0 {
		return nil, false
	}
	frame := q.tasks[0]
	q.tasks[0] = nil
	q.tasks = q.tasks[1:]
	q.count--
	if q.count == 0 {
		// Force a GC of the underlying array, since it might have
		// grown significantly if the queue was hammered for some reason
		q.tasks = nil
	}
	return frame, true
}

type inputTask struct {
	ctx      context.Context
	t        *txnReq
	event    *gomatrixserverlib.HeaderedEvent
	wg       *sync.WaitGroup
	err      error         // written back by worker, only safe to read when all tasks are done
	duration time.Duration // written back by worker, only safe to read when all tasks are done
}

type inputWorker struct {
	running atomic.Bool
	input   *sendFIFOQueue
}

var inFlightTxnsPerOrigin sync.Map // transaction ID -> chan util.JSONResponse
var inputWorkers sync.Map          // room ID -> *inputWorker

// Send implements /_matrix/federation/v1/send/{txnID}
func Send(
	httpReq *http.Request,
	request *gomatrixserverlib.FederationRequest,
	txnID gomatrixserverlib.TransactionID,
	cfg *config.FederationAPI,
	rsAPI api.RoomserverInternalAPI,
	eduAPI eduserverAPI.EDUServerInputAPI,
	keyAPI keyapi.KeyInternalAPI,
	keys gomatrixserverlib.JSONVerifier,
	federation *gomatrixserverlib.FederationClient,
	mu *internal.MutexByRoom,
	servers federationAPI.ServersInRoomProvider,
) util.JSONResponse {
	// First we should check if this origin has already submitted this
	// txn ID to us. If they have and the txnIDs map contains an entry,
	// the transaction is still being worked on. The new client can wait
	// for it to complete rather than creating more work.
	index := string(request.Origin()) + "\000" + string(txnID)
	v, ok := inFlightTxnsPerOrigin.LoadOrStore(index, make(chan util.JSONResponse, 1))
	ch := v.(chan util.JSONResponse)
	if ok {
		// This origin already submitted this txn ID to us, and the work
		// is still taking place, so we'll just wait for it to finish.
		ctx, cancel := context.WithTimeout(httpReq.Context(), time.Minute*5)
		defer cancel()
		select {
		case <-ctx.Done():
			// If the caller gives up then return straight away. We don't
			// want to attempt to process what they sent us any further.
			return util.JSONResponse{Code: http.StatusRequestTimeout}
		case res := <-ch:
			// The original task just finished processing so let's return
			// the result of it.
			if res.Code == 0 {
				return util.JSONResponse{Code: http.StatusAccepted}
			}
			return res
		}
	}
	// Otherwise, store that we're currently working on this txn from
	// this origin. When we're done processing, close the channel.
	defer close(ch)
	defer inFlightTxnsPerOrigin.Delete(index)

	t := txnReq{
		rsAPI:      rsAPI,
		eduAPI:     eduAPI,
		keys:       keys,
		federation: federation,
		servers:    servers,
		keyAPI:     keyAPI,
		roomsMu:    mu,
	}

	var txnEvents struct {
		PDUs []json.RawMessage       `json:"pdus"`
		EDUs []gomatrixserverlib.EDU `json:"edus"`
	}

	if err := json.Unmarshal(request.Content(), &txnEvents); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: jsonerror.NotJSON("The request body could not be decoded into valid JSON. " + err.Error()),
		}
	}
	// Transactions are limited in size; they can have at most 50 PDUs and 100 EDUs.
	// https://matrix.org/docs/spec/server_server/latest#transactions
	if len(txnEvents.PDUs) > 50 || len(txnEvents.EDUs) > 100 {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: jsonerror.BadJSON("max 50 pdus / 100 edus"),
		}
	}

	// TODO: Really we should have a function to convert FederationRequest to txnReq
	t.PDUs = txnEvents.PDUs
	t.EDUs = txnEvents.EDUs
	t.Origin = request.Origin()
	t.TransactionID = txnID
	t.Destination = cfg.Matrix.ServerName

	util.GetLogger(httpReq.Context()).Infof("Received transaction %q from %q containing %d PDUs, %d EDUs", txnID, request.Origin(), len(t.PDUs), len(t.EDUs))

	resp, jsonErr := t.processTransaction(context.Background())
	if jsonErr != nil {
		util.GetLogger(httpReq.Context()).WithField("jsonErr", jsonErr).Error("t.processTransaction failed")
		return *jsonErr
	}

	// https://matrix.org/docs/spec/server_server/r0.1.3#put-matrix-federation-v1-send-txnid
	// Status code 200:
	// The result of processing the transaction. The server is to use this response
	// even in the event of one or more PDUs failing to be processed.
	res := util.JSONResponse{
		Code: http.StatusOK,
		JSON: resp,
	}
	ch <- res
	return res
}

type txnReq struct {
	gomatrixserverlib.Transaction
	rsAPI      api.RoomserverInternalAPI
	eduAPI     eduserverAPI.EDUServerInputAPI
	keyAPI     keyapi.KeyInternalAPI
	keys       gomatrixserverlib.JSONVerifier
	federation txnFederationClient
	roomsMu    *internal.MutexByRoom
	servers    federationAPI.ServersInRoomProvider
	work       string
}

// A subset of FederationClient functionality that txn requires. Useful for testing.
type txnFederationClient interface {
	LookupState(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, eventID string, roomVersion gomatrixserverlib.RoomVersion) (
		res gomatrixserverlib.RespState, err error,
	)
	LookupStateIDs(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, eventID string) (res gomatrixserverlib.RespStateIDs, err error)
	GetEvent(ctx context.Context, s gomatrixserverlib.ServerName, eventID string) (res gomatrixserverlib.Transaction, err error)
	LookupMissingEvents(ctx context.Context, s gomatrixserverlib.ServerName, roomID string, missing gomatrixserverlib.MissingEvents,
		roomVersion gomatrixserverlib.RoomVersion) (res gomatrixserverlib.RespMissingEvents, err error)
}

func (t *txnReq) processTransaction(ctx context.Context) (*gomatrixserverlib.RespSend, *util.JSONResponse) {
	results := make(map[string]gomatrixserverlib.PDUResult)
	var wg sync.WaitGroup
	var tasks []*inputTask

	for _, pdu := range t.PDUs {
		pduCountTotal.WithLabelValues("total").Inc()
		var header struct {
			RoomID string `json:"room_id"`
		}
		if err := json.Unmarshal(pdu, &header); err != nil {
			util.GetLogger(ctx).WithError(err).Warn("Transaction: Failed to extract room ID from event")
			// We don't know the event ID at this point so we can't return the
			// failure in the PDU results
			continue
		}
		verReq := api.QueryRoomVersionForRoomRequest{RoomID: header.RoomID}
		verRes := api.QueryRoomVersionForRoomResponse{}
		if err := t.rsAPI.QueryRoomVersionForRoom(ctx, &verReq, &verRes); err != nil {
			util.GetLogger(ctx).WithError(err).Warn("Transaction: Failed to query room version for room", verReq.RoomID)
			// We don't know the event ID at this point so we can't return the
			// failure in the PDU results
			continue
		}
		event, err := gomatrixserverlib.NewEventFromUntrustedJSON(pdu, verRes.RoomVersion)
		if err != nil {
			if _, ok := err.(gomatrixserverlib.BadJSONError); ok {
				// Room version 6 states that homeservers should strictly enforce canonical JSON
				// on PDUs.
				//
				// This enforces that the entire transaction is rejected if a single bad PDU is
				// sent. It is unclear if this is the correct behaviour or not.
				//
				// See https://github.com/matrix-org/synapse/issues/7543
				return nil, &util.JSONResponse{
					Code: 400,
					JSON: jsonerror.BadJSON("PDU contains bad JSON"),
				}
			}
			util.GetLogger(ctx).WithError(err).Warnf("Transaction: Failed to parse event JSON of event %s", string(pdu))
			continue
		}
		if api.IsServerBannedFromRoom(ctx, t.rsAPI, event.RoomID(), t.Origin) {
			results[event.EventID()] = gomatrixserverlib.PDUResult{
				Error: "Forbidden by server ACLs",
			}
			continue
		}
		if err = event.VerifyEventSignatures(ctx, t.keys); err != nil {
			util.GetLogger(ctx).WithError(err).Warnf("Transaction: Couldn't validate signature of event %q", event.EventID())
			results[event.EventID()] = gomatrixserverlib.PDUResult{
				Error: err.Error(),
			}
			continue
		}
		v, _ := inputWorkers.LoadOrStore(event.RoomID(), &inputWorker{
			input: newSendFIFOQueue(),
		})
		worker := v.(*inputWorker)
		wg.Add(1)
		task := &inputTask{
			ctx:   ctx,
			t:     t,
			event: event.Headered(verRes.RoomVersion),
			wg:    &wg,
		}
		tasks = append(tasks, task)
		worker.input.push(task)
		if worker.running.CAS(false, true) {
			go worker.run()
		}
	}

	t.processEDUs(ctx)
	wg.Wait()

	for _, task := range tasks {
		if task.err != nil {
			results[task.event.EventID()] = gomatrixserverlib.PDUResult{
				//	Error: task.err.Error(), TODO: this upsets tests if uncommented
			}
		} else {
			results[task.event.EventID()] = gomatrixserverlib.PDUResult{}
		}
	}

	if c := len(results); c > 0 {
		util.GetLogger(ctx).Debugf("Processed %d PDUs from %v in transaction %q", c, t.Origin, t.TransactionID)
	}
	return &gomatrixserverlib.RespSend{PDUs: results}, nil
}

func (t *inputWorker) run() {
	defer t.running.Store(false)
	for {
		task, ok := t.input.pop()
		if !ok {
			return
		}
		if task == nil {
			continue
		}
		func() {
			defer task.wg.Done()
			select {
			case <-task.ctx.Done():
				task.err = context.DeadlineExceeded
				pduCountTotal.WithLabelValues("expired").Inc()
				return
			default:
				evStart := time.Now()
				// TODO: Is 5 minutes too long?
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
				task.err = task.t.processEvent(ctx, task.event)
				cancel()
				task.duration = time.Since(evStart)
				if err := task.err; err != nil {
					switch err.(type) {
					case *gomatrixserverlib.NotAllowed:
						processEventSummary.WithLabelValues(task.t.work, MetricsOutcomeRejected).Observe(
							float64(time.Since(evStart).Nanoseconds()) / 1000.,
						)
						util.GetLogger(task.ctx).WithError(err).WithField("event_id", task.event.EventID()).WithField("rejected", true).Warn(
							"Failed to process incoming federation event, skipping",
						)
						task.err = nil // make "rejected" failures silent
					default:
						processEventSummary.WithLabelValues(task.t.work, MetricsOutcomeFail).Observe(
							float64(time.Since(evStart).Nanoseconds()) / 1000.,
						)
						util.GetLogger(task.ctx).WithError(err).WithField("event_id", task.event.EventID()).WithField("rejected", false).Warn(
							"Failed to process incoming federation event, skipping",
						)
					}
				} else {
					pduCountTotal.WithLabelValues("success").Inc()
					processEventSummary.WithLabelValues(task.t.work, MetricsOutcomeOK).Observe(
						float64(time.Since(evStart).Nanoseconds()) / 1000.,
					)
				}
			}
		}()
	}
}

func (t *txnReq) processEDUs(ctx context.Context) {
	for _, e := range t.EDUs {
		eduCountTotal.Inc()
		switch e.Type {
		case gomatrixserverlib.MTyping:
			// https://matrix.org/docs/spec/server_server/latest#typing-notifications
			var typingPayload struct {
				RoomID string `json:"room_id"`
				UserID string `json:"user_id"`
				Typing bool   `json:"typing"`
			}
			if err := json.Unmarshal(e.Content, &typingPayload); err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to unmarshal typing event")
				continue
			}
			_, domain, err := gomatrixserverlib.SplitID('@', typingPayload.UserID)
			if err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to split domain from typing event sender")
				continue
			}
			if domain != t.Origin {
				util.GetLogger(ctx).Warnf("Dropping typing event where sender domain (%q) doesn't match origin (%q)", domain, t.Origin)
				continue
			}
			if err := eduserverAPI.SendTyping(ctx, t.eduAPI, typingPayload.UserID, typingPayload.RoomID, typingPayload.Typing, 30*1000); err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to send typing event to edu server")
			}
		case gomatrixserverlib.MDirectToDevice:
			// https://matrix.org/docs/spec/server_server/r0.1.3#m-direct-to-device-schema
			var directPayload gomatrixserverlib.ToDeviceMessage
			if err := json.Unmarshal(e.Content, &directPayload); err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to unmarshal send-to-device events")
				continue
			}
			for userID, byUser := range directPayload.Messages {
				for deviceID, message := range byUser {
					// TODO: check that the user and the device actually exist here
					if err := eduserverAPI.SendToDevice(ctx, t.eduAPI, directPayload.Sender, userID, deviceID, directPayload.Type, message); err != nil {
						util.GetLogger(ctx).WithError(err).WithFields(logrus.Fields{
							"sender":    directPayload.Sender,
							"user_id":   userID,
							"device_id": deviceID,
						}).Error("Failed to send send-to-device event to edu server")
					}
				}
			}
		case gomatrixserverlib.MDeviceListUpdate:
			t.processDeviceListUpdate(ctx, e)
		case gomatrixserverlib.MReceipt:
			// https://matrix.org/docs/spec/server_server/r0.1.4#receipts
			payload := map[string]eduserverAPI.FederationReceiptMRead{}

			if err := json.Unmarshal(e.Content, &payload); err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to unmarshal receipt event")
				continue
			}

			for roomID, receipt := range payload {
				for userID, mread := range receipt.User {
					_, domain, err := gomatrixserverlib.SplitID('@', userID)
					if err != nil {
						util.GetLogger(ctx).WithError(err).Error("Failed to split domain from receipt event sender")
						continue
					}
					if t.Origin != domain {
						util.GetLogger(ctx).Warnf("Dropping receipt event where sender domain (%q) doesn't match origin (%q)", domain, t.Origin)
						continue
					}
					if err := t.processReceiptEvent(ctx, userID, roomID, "m.read", mread.Data.TS, mread.EventIDs); err != nil {
						util.GetLogger(ctx).WithError(err).WithFields(logrus.Fields{
							"sender":  t.Origin,
							"user_id": userID,
							"room_id": roomID,
							"events":  mread.EventIDs,
						}).Error("Failed to send receipt event to edu server")
						continue
					}
				}
			}
		case eduserverAPI.MSigningKeyUpdate:
			var updatePayload eduserverAPI.CrossSigningKeyUpdate
			if err := json.Unmarshal(e.Content, &updatePayload); err != nil {
				util.GetLogger(ctx).WithError(err).WithFields(logrus.Fields{
					"user_id": updatePayload.UserID,
				}).Error("Failed to send signing key update to edu server")
				continue
			}
			inputReq := &eduserverAPI.InputCrossSigningKeyUpdateRequest{
				CrossSigningKeyUpdate: updatePayload,
			}
			inputRes := &eduserverAPI.InputCrossSigningKeyUpdateResponse{}
			if err := t.eduAPI.InputCrossSigningKeyUpdate(ctx, inputReq, inputRes); err != nil {
				util.GetLogger(ctx).WithError(err).Error("Failed to unmarshal cross-signing update")
				continue
			}
		default:
			util.GetLogger(ctx).WithField("type", e.Type).Debug("Unhandled EDU")
		}
	}
}

// processReceiptEvent sends receipt events to the edu server
func (t *txnReq) processReceiptEvent(ctx context.Context,
	userID, roomID, receiptType string,
	timestamp gomatrixserverlib.Timestamp,
	eventIDs []string,
) error {
	// store every event
	for _, eventID := range eventIDs {
		req := eduserverAPI.InputReceiptEventRequest{
			InputReceiptEvent: eduserverAPI.InputReceiptEvent{
				UserID:    userID,
				RoomID:    roomID,
				EventID:   eventID,
				Type:      receiptType,
				Timestamp: timestamp,
			},
		}
		resp := eduserverAPI.InputReceiptEventResponse{}
		if err := t.eduAPI.InputReceiptEvent(ctx, &req, &resp); err != nil {
			return fmt.Errorf("unable to set receipt event: %w", err)
		}
	}

	return nil
}

func (t *txnReq) processDeviceListUpdate(ctx context.Context, e gomatrixserverlib.EDU) {
	var payload gomatrixserverlib.DeviceListUpdateEvent
	if err := json.Unmarshal(e.Content, &payload); err != nil {
		util.GetLogger(ctx).WithError(err).Error("Failed to unmarshal device list update event")
		return
	}
	var inputRes keyapi.InputDeviceListUpdateResponse
	t.keyAPI.InputDeviceListUpdate(context.Background(), &keyapi.InputDeviceListUpdateRequest{
		Event: payload,
	}, &inputRes)
	if inputRes.Error != nil {
		util.GetLogger(ctx).WithError(inputRes.Error).WithField("user_id", payload.UserID).Error("failed to InputDeviceListUpdate")
	}
}

func (t *txnReq) processEvent(_ context.Context, e *gomatrixserverlib.HeaderedEvent) error {
	// pass the event to the roomserver which will do auth checks
	// If the event fail auth checks, gmsl.NotAllowed error will be returned which we be silently
	// discarded by the caller of this function
	return api.SendEvents(
		context.Background(),
		t.rsAPI,
		api.KindNew,
		[]*gomatrixserverlib.HeaderedEvent{e},
		t.Origin,
		api.DoNotSendToOtherServers,
		nil,
		false,
	)
}
