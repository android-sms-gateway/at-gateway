package inbox

import (
	"context"
	"encoding/base64"
	"strings"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

// Service is the inbound SMS pipeline: it consumes the decoded SMS-DELIVER
// parts the modem module hands off, reassembles concatenated messages, damps
// modem re-notifications per webhook and emits the assembled messages as
// inbound webhook events. Every assembled message is also PERSISTED in the
// local inbox store, so the stored rows can be listed and replayed later.
type Service struct {
	config Config

	inbox *Repository

	devicesSvc  *devices.Service
	modemSvc    *modem.Service
	webhooksSvc *webhooks.Service

	// inboundGroups holds the parts of every INCOMPLETE concatenated inbound
	// message, keyed by (sender, reference width, reference). It lives next to
	// the consumer because only the Run loop goroutine touches it.
	inboundGroups map[inboundKey]*inboundGroup
	// inboundDedup damps modem re-notifications per webhook (see
	// inboundDeduper).
	inboundDedup *inboundDeduper
	// startedAt marks the process start; together with inboundPostRestart it
	// drives the post-restart observability window.
	startedAt time.Time
	// inboundPostRestart is true while the process is inside the post-start
	// observation window, i.e. while the dedupe has no memory of a previous
	// process.
	inboundPostRestart bool

	metrics *Metrics

	logger *zap.Logger
}

// NewService wires the inbound pipeline with its configuration and the inbox
// store, device, modem, webhook, metrics and logger dependencies. A
// non-positive ReassemblyTimeout falls back to five minutes and a non-positive
// SweepInterval to one second; the returned service is driven by Run.
func NewService(
	config Config,
	inbox *Repository,
	devicesSvc *devices.Service,
	modemSvc *modem.Service,
	webhooksSvc *webhooks.Service,
	metrics *Metrics,
	logger *zap.Logger,
) *Service {
	if config.ReassemblyTimeout <= 0 {
		config.ReassemblyTimeout = defaultReassemblyTimeout
	}
	if config.SweepInterval <= 0 {
		config.SweepInterval = defaultSweepInterval
	}

	return &Service{
		config: config,

		inbox: inbox,

		devicesSvc:  devicesSvc,
		modemSvc:    modemSvc,
		webhooksSvc: webhooksSvc,

		inboundGroups:      make(map[inboundKey]*inboundGroup),
		inboundDedup:       newInboundDeduper(),
		startedAt:          time.Now().UTC(),
		inboundPostRestart: true,

		metrics: metrics,

		logger: logger,
	}
}

// Run consumes the modem's inbound parts until ctx is canceled: a complete
// message is emitted immediately, an incomplete concatenation stays buffered
// until every part arrived, and the reassembly/dedupe eviction runs on this
// module's OWN ticker (config.SweepInterval), independent of the messages
// poll interval. When the context is done the groups still incomplete are
// abort-accounted - their remaining parts can never complete in this process -
// and Run returns nil.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.config.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.abortInbound()
			return nil
		case part := <-s.modemSvc.InboundParts():
			s.handleInboundPart(ctx, part, time.Now().UTC())
		case <-ticker.C:
			s.sweepInbound(time.Now().UTC())
		}
	}
}

// List returns one page of the stored inbound messages plus the total number
// of rows matching the filter.
//
// The listing is ALWAYS owned by the local device: the gateway is
// single-device, so an explicitly supplied device id is a VALIDATION (a
// foreign one yields ErrDeviceNotFound), never a row filter.
func (s *Service) List(ctx context.Context, options ListOptions) ([]IncomingMessage, int, error) {
	deviceID := s.devicesSvc.Get().ID

	var filter ListFilter
	if options.Filter != nil {
		filter = *options.Filter
	}

	if err := filter.validate(deviceID); err != nil {
		return nil, 0, err
	}

	filter.DeviceID = &deviceID

	result, total, err := s.inbox.List(ctx, ListOptions{
		Filter:     &filter,
		Pagination: options.Pagination,
	})
	if err != nil {
		return nil, 0, err
	}

	return result, total, nil
}

// handleInboundPart consumes one decoded SMS-DELIVER part: a single-part
// message is emitted immediately, a concatenated one is buffered until every
// part arrived. It runs on the Run loop goroutine, so the reassembly state
// needs no locking. The context is the Run context: it bounds the store write
// of a complete message.
func (s *Service) handleInboundPart(ctx context.Context, part modem.InboundPart, now time.Time) {
	message, complete := s.assembleInbound(part, now)
	if !complete {
		s.logger.Debug("inbound SMS part buffered",
			zap.Int("sequence", part.Concat.Sequence),
			zap.Int("segments", part.Concat.Segments),
			zap.Int("ref", part.Concat.Ref),
			zap.Int("ref_width", part.Concat.RefWidth),
		)

		return
	}

	s.emitInbound(ctx, message, now)
}

// assembleInbound merges a part into its reassembly group and reports whether
// the message is complete. A single-part message (no concatenation IE) is
// always complete.
func (s *Service) assembleInbound(part modem.InboundPart, now time.Time) (inboundMessage, bool) {
	if part.Concat == nil {
		return inboundMessage{
			sender:         part.Sender,
			content:        part.Content,
			ported:         part.Ported,
			port:           part.Port,
			receivedAt:     part.ReceivedAt,
			timestampValid: part.TimestampValid,
			concat:         nil,
		}, true
	}

	key := inboundKey{sender: part.Sender, refWidth: part.Concat.RefWidth, ref: part.Concat.Ref}
	group, known := s.inboundGroups[key]
	switch {
	case !known:
		group = s.newInboundGroup(part.Concat.Segments, now)
		s.inboundGroups[key] = group
	case group.segments != part.Concat.Segments:
		// The service centre reused the reference for a differently sized
		// message: the old group can never complete, so restart it.
		s.logger.Warn("inbound concat reference reused with a different part count",
			zap.Int("ref", part.Concat.Ref),
			zap.Int("ref_width", part.Concat.RefWidth),
			zap.Int("segments", part.Concat.Segments),
		)
		group = s.newInboundGroup(part.Concat.Segments, now)
		s.inboundGroups[key] = group
	}

	// A repeated sequence number overwrites its predecessor: the modem
	// re-notifies the same index, not a new part.
	group.parts[part.Concat.Sequence] = part
	if len(group.parts) < group.segments {
		return inboundMessage{}, false
	}

	contents := make([]string, 0, group.segments)
	for sequence := 1; sequence <= group.segments; sequence++ {
		piece, ok := group.parts[sequence]
		if !ok {
			// Unreachable while len(parts) == segments and every sequence is
			// 1-based, but a partial message must never be emitted.
			return inboundMessage{}, false
		}
		contents = append(contents, piece.Content)
	}

	first, ok := group.parts[1]
	if !ok {
		return inboundMessage{}, false
	}
	delete(s.inboundGroups, key)

	return inboundMessage{
		sender:         first.Sender,
		content:        strings.Join(contents, ""),
		ported:         first.Ported,
		port:           first.Port,
		receivedAt:     first.ReceivedAt,
		timestampValid: first.TimestampValid,
		concat:         first.Concat,
	}, true
}

// newInboundGroup creates an empty group, evicting the oldest one when the
// concurrent-group bound is already reached (so a stream of half-delivered
// concatenations cannot grow the map without limit). The bound check first
// runs the eviction sweep, so an expired group never causes a capacity
// eviction.
func (s *Service) newInboundGroup(segments int, now time.Time) *inboundGroup {
	if len(s.inboundGroups) >= inboundMaxGroups {
		s.sweepInbound(now)
	}
	if len(s.inboundGroups) >= inboundMaxGroups {
		s.logger.Warn("inbound reassembly group capacity reached, evicting the oldest group",
			zap.Int("capacity", inboundMaxGroups),
		)
		s.evictOldestInboundGroup()
	}

	return &inboundGroup{segments: segments, firstAt: now, parts: make(map[int]modem.InboundPart)}
}

// evictOldestInboundGroup drops the oldest incomplete group and counts it.
func (s *Service) evictOldestInboundGroup() {
	oldest := inboundKey{sender: "", refWidth: 0, ref: 0}
	found := false
	var oldestAt time.Time
	for key, group := range s.inboundGroups {
		if !found || group.firstAt.Before(oldestAt) {
			oldest, oldestAt, found = key, group.firstAt, true
		}
	}
	if found {
		delete(s.inboundGroups, oldest)
		s.metrics.IncReassemblyTimeout()
	}
}

// sweepInbound evicts the groups whose window elapsed and forgets the dedupe
// entries whose TTL elapsed. It runs on the poll ticker.
func (s *Service) sweepInbound(now time.Time) {
	for key, group := range s.inboundGroups {
		if now.Sub(group.firstAt) < s.config.ReassemblyTimeout {
			continue
		}
		delete(s.inboundGroups, key)
		s.metrics.IncReassemblyTimeout()
	}

	s.inboundDedup.sweep(now)
}

// abortInbound counts the groups still incomplete when the worker stops; their
// remaining parts can never complete in this process.
func (s *Service) abortInbound() {
	for range s.inboundGroups {
		s.metrics.IncReassemblyAborted()
	}
	s.inboundGroups = make(map[inboundKey]*inboundGroup)
}

// emitInbound emits one assembled message and records the outcome in the
// dedupe. Marking happens PER WEBHOOK and only for webhooks whose queue item
// was actually stored, so a failed enqueue is always retried and a served
// webhook never receives a duplicate.
//
// The message is PERSISTED BEFORE the emission: the stored row is the durable
// record, the emission is the notification. A persist failure never aborts the
// emission - it is counted and logged instead.
func (s *Service) emitInbound(ctx context.Context, message inboundMessage, now time.Time) {
	messageID := inboundMessageID(message)
	served := s.inboundDedup.served(messageID, now)

	receivedAt := message.receivedAt
	if !message.timestampValid {
		// The modem reported no usable TP-SCTS. The wire payload has a
		// non-optional timestamp, so the observation time is stamped - but the
		// message identity stays stamp-free, keeping it stable across
		// re-notifications and restarts.
		receivedAt = now
	}

	content := inboundContent(message)
	recipient := lo.EmptyableToPtr(s.modemSvc.SIM().PhoneNumber)
	simNumber := lo.ToPtr(inboundSIMNumber)

	s.persistInbound(ctx, IncomingMessage{
		ID:         messageID,
		DeviceID:   s.devicesSvc.Get().ID,
		Type:       inboundMessageType(message),
		Sender:     message.sender,
		Recipient:  recipient,
		SimNumber:  simNumber,
		Content:    content,
		ReceivedAt: receivedAt,
	})

	event := webhooks.InboundEvent{
		MessageID: messageID,
		Sender:    message.sender,
		Recipient: recipient,
		SimNumber: simNumber,
		Message:   "",
		Data:      "",
		At:        receivedAt,
	}

	var (
		out       webhooks.TrackedEmission
		eventName smsgateway.WebhookEvent
	)
	if message.ported {
		eventName = smsgateway.WebhookEventSmsDataReceived
		event.Data = content
		out = s.webhooksSvc.EmitSmsDataReceived(event, served)
	} else {
		eventName = smsgateway.WebhookEventSmsReceived
		event.Message = content
		out = s.webhooksSvc.EmitSmsReceived(event, served)
	}
	s.metrics.IncInboundEvent(eventName)

	if out.Suppressed > 0 {
		s.metrics.IncDedupeBlocked()
		s.logger.Debug("inbound emission suppressed by the dedupe",
			zap.String("message_id", messageID),
			zap.Int("suppressed", out.Suppressed),
		)
	}

	for _, result := range out.Results {
		if result.Err == nil {
			s.inboundDedup.mark(messageID, result.WebhookID, now)
		}
	}

	s.countPostRestartEmission(len(out.Results), out.Suppressed, now)
}

// countPostRestartEmission counts the emissions the in-memory dedupe CANNOT
// have suppressed because it was built after the process started. Inside the
// post-start window (the reassembly timeout) every accepted emission may be a
// still-stored message whose delivery cycle restarts from scratch after the
// restart, so the counter is the observable of that blast.
func (s *Service) countPostRestartEmission(emitted, suppressed int, now time.Time) {
	if !s.inboundPostRestart {
		return
	}
	if now.Sub(s.startedAt) >= s.config.ReassemblyTimeout {
		s.inboundPostRestart = false

		return
	}
	if emitted > 0 && suppressed == 0 {
		s.metrics.IncDedupeSkippedPostRestart()
	}
}

// persistInbound stores one assembled message in the local inbox store.
//
// The insert is BOUNDED by a short timeout derived from the Run context: the
// SQLite connection is shared by every writer and the inbound pipeline has
// exactly ONE consumer, so an unbounded insert could stall the consumer until
// the modem hand-off budget sheds the message.
//
// A failed or timed-out insert is counted and logged, and NEVER aborts the
// webhook emission: the emission is the notification, the row is the durable
// record. The insert runs on the consumer goroutine - never while the modem
// command lock is held - and the modem path is never blocked on the store,
// because the part was already handed off.
func (s *Service) persistInbound(ctx context.Context, message IncomingMessage) {
	storeCtx, cancel := context.WithTimeout(ctx, inboundStoreTimeout)
	defer cancel()

	if err := s.inbox.Insert(storeCtx, message); err != nil {
		s.metrics.IncStoreFailed()
		// The sender and the content are user PII: they are never logged
		// above Debug, so only the message identity is reported here.
		s.logger.Info("inbound message not stored",
			zap.String("message_id", message.ID),
			zap.Error(err),
		)

		return
	}

	s.metrics.IncStored()
}

// inboundContent returns the WIRE-READY body of an assembled message: the
// assembled text for an SMS, the base64 NO_WRAP encoding of the raw octets for
// a DATA_SMS. The data encoding is byte-identical to the data field of the
// sms:data-received webhook payload, so a stored row replays without any
// re-encoding.
func inboundContent(message inboundMessage) string {
	if message.ported {
		return base64.StdEncoding.EncodeToString([]byte(message.content))
	}

	return message.content
}

// inboundMessageType returns the stored type of an assembled message. Only SMS
// and DATA_SMS exist at this gateway: MMS is never received over AT.
func inboundMessageType(message inboundMessage) smsgateway.IncomingMessageType {
	if message.ported {
		return smsgateway.IncomingMessageTypeDataSMS
	}

	return smsgateway.IncomingMessageTypeSMS
}
