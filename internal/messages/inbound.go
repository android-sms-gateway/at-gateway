package messages

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

const (
	// inboundSIMNumber is the only SIM at-gateway supports.
	inboundSIMNumber uint8 = 1

	// inboundDedupeCapacity bounds how many message ids the dedupe remembers;
	// the oldest entry is evicted once the bound is reached.
	inboundDedupeCapacity = 1024

	// inboundDedupeTTL is how long a message id stays suppressed after its
	// last webhook was served. It is deliberately SHORTER than the webhook
	// retry window (RetryCount 15 at a 5s base = ~45h): the dedupe is a modem
	// re-notification damper INSIDE the window, not a delivery guarantee.
	// See dev-docs/at-gateway for the at-least-once contract.
	inboundDedupeTTL = 10 * time.Minute

	// inboundMaxGroups bounds the number of concurrently incomplete
	// reassembly groups, so a stream of half-delivered concatenations cannot
	// grow the map without limit.
	inboundMaxGroups = 512

	// inboundIDSeparator joins the components of the message identity; the
	// unit separator cannot occur in any component.
	inboundIDSeparator = "\x1f"

	// inboundIDLength is the length of the message identity in hex chars (the
	// first half of a SHA-256).
	inboundIDLength = 16
)

// inboundKey identifies one concatenated message. The reference WIDTH is part
// of the key, so a 0x42 8-bit reference and a 0x0042 16-bit reference from the
// same sender never merge.
type inboundKey struct {
	sender   string
	refWidth int
	ref      int
}

// inboundGroup collects the parts of one concatenated message until it is
// complete or evicted.
type inboundGroup struct {
	// segments is the part count the service centre announced; a part that
	// announces a different count restarts the group.
	segments int
	// firstAt is when the group was created, i.e. when its eviction window
	// started.
	firstAt time.Time
	// parts holds the decoded parts by their 1-based sequence number.
	parts map[int]modem.InboundPart
}

// inboundMessage is one ASSEMBLED inbound message: the unit of emission.
type inboundMessage struct {
	// sender is the originating address, taken from the FIRST part (every
	// part of a concatenation repeats it).
	sender string
	// content is the assembled text, or the assembled raw octets as a string
	// for a port-addressed data SMS.
	content string
	// ported reports that a destination-port IE was present, which is what
	// makes the message DATA rather than text.
	ported bool
	// port is the destination port; zero without a port IE.
	port uint16
	// receivedAt is the TP-SCTS of the first part, in UTC.
	receivedAt time.Time
	// timestampValid reports whether receivedAt is usable; when false the
	// message identity omits the stamp.
	timestampValid bool
	// concat is the concatenation descriptor of the message; nil for a
	// single-part message.
	concat *modem.ConcatRef
}

// inboundDedupEntry is the per-message-id record of the webhooks that already
// hold a queue item for it.
type inboundDedupEntry struct {
	webhooks map[string]struct{}
	seenAt   time.Time
}

// inboundDeduper is a bounded, TTL-based, PER-WEBHOOK dedupe of emitted
// message ids. It damps a modem that keeps re-notifying an undeleted store
// index: the same message id is re-emitted only to the webhooks whose enqueue
// failed, and only inside the TTL.
//
// It is per process and in memory: a restart re-arms it (see
// IncDedupeSkippedPostRestart). Only the Run loop touches it.
type inboundDeduper struct {
	entries  map[string]inboundDedupEntry
	order    []string
	capacity int
	ttl      time.Duration
}

// newInboundDeduper builds a deduper with the pinned capacity and TTL.
func newInboundDeduper() *inboundDeduper {
	return &inboundDeduper{
		entries:  make(map[string]inboundDedupEntry),
		order:    make([]string, 0, inboundDedupeCapacity),
		capacity: inboundDedupeCapacity,
		ttl:      inboundDedupeTTL,
	}
}

// served returns the webhooks already served for messageID inside the TTL, in
// no particular order. An expired entry serves nothing (and is forgotten).
func (d *inboundDeduper) served(messageID string, now time.Time) []string {
	entry, ok := d.entries[messageID]
	if !ok || now.Sub(entry.seenAt) >= d.ttl {
		return nil
	}

	served := make([]string, 0, len(entry.webhooks))
	for id := range entry.webhooks {
		served = append(served, id)
	}

	return served
}

// mark records that webhookID now holds a queue item for messageID. Marking is
// PER WEBHOOK and happens only AFTER a successful enqueue, so a failed enqueue
// is always retried for that webhook.
func (d *inboundDeduper) mark(messageID, webhookID string, now time.Time) {
	entry, ok := d.entries[messageID]
	if !ok {
		if len(d.entries) >= d.capacity {
			d.evictOldest()
		}
		entry = inboundDedupEntry{webhooks: make(map[string]struct{}), seenAt: now}
		d.entries[messageID] = entry
		d.order = append(d.order, messageID)
	}

	entry.webhooks[webhookID] = struct{}{}
	entry.seenAt = now
	d.entries[messageID] = entry
}

// sweep forgets the entries whose TTL elapsed.
func (d *inboundDeduper) sweep(now time.Time) {
	kept := make([]string, 0, len(d.order))
	for _, messageID := range d.order {
		entry, ok := d.entries[messageID]
		if !ok {
			continue
		}
		if now.Sub(entry.seenAt) >= d.ttl {
			delete(d.entries, messageID)

			continue
		}
		kept = append(kept, messageID)
	}
	d.order = kept
}

// evictOldest drops the oldest remembered message id, bounding the memory.
func (d *inboundDeduper) evictOldest() {
	for len(d.order) > 0 {
		oldest := d.order[0]
		d.order = d.order[1:]
		if _, ok := d.entries[oldest]; ok {
			delete(d.entries, oldest)

			return
		}
	}
}

// handleInboundPart consumes one decoded SMS-DELIVER part: a single-part
// message is emitted immediately, a concatenated one is buffered until every
// part arrived. It runs on the Run loop goroutine, so the reassembly state
// needs no locking.
func (s *Service) handleInboundPart(part modem.InboundPart, now time.Time) {
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

	s.emitInbound(message, now)
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
		if now.Sub(group.firstAt) < s.config.Inbound.ReassemblyTimeout {
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
func (s *Service) emitInbound(message inboundMessage, now time.Time) {
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

	event := webhooks.InboundEvent{
		MessageID: messageID,
		Sender:    message.sender,
		Recipient: lo.EmptyableToPtr(s.modemSvc.SIM().PhoneNumber),
		SimNumber: lo.ToPtr(inboundSIMNumber),
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
		event.Data = base64.StdEncoding.EncodeToString([]byte(message.content))
		out = s.webhooksSvc.EmitSmsDataReceived(event, served)
	} else {
		eventName = smsgateway.WebhookEventSmsReceived
		event.Message = message.content
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
	if now.Sub(s.startedAt) >= s.config.Inbound.ReassemblyTimeout {
		s.inboundPostRestart = false

		return
	}
	if emitted > 0 && suppressed == 0 {
		s.metrics.IncDedupeSkippedPostRestart()
	}
}

// inboundMessageID is the content-deterministic identity of an assembled
// inbound message: the first 16 hex chars of a SHA-256 over the sender, the
// service-centre stamp (when the modem reported a usable one), the assembled
// content, the destination port and the concatenation reference.
//
// The identity is what CONSUMERS deduplicate on, because webhook delivery is
// AT-LEAST-ONCE: a re-notification, a retry and a post-restart cycle all carry
// the same id. When the TP-SCTS is absent or implausible the stamp component
// is OMITTED (not zeroed), so the id stays stable across re-notifications;
// the accepted consequence is that byte-identical content from the same sender
// inside the dedupe window collapses to one id (counted by
// at_gateway_messages_dedupe_blocked_total).
//
// This 16-hex-char form DIVERGES from the Android gateway's 8-hex-char
// hashCode form; both are deterministic per message, and the divergence is
// documented in dev-docs/at-gateway.
func inboundMessageID(message inboundMessage) string {
	components := []string{message.sender, message.content, strconv.Itoa(int(message.port))}
	if message.concat != nil {
		components = append(
			components,
			strconv.Itoa(message.concat.RefWidth)+":"+strconv.Itoa(message.concat.Ref),
		)
	}
	if message.timestampValid {
		components = append(
			[]string{message.receivedAt.UTC().Format(time.RFC3339)},
			components...,
		)
	}

	sum := sha256.Sum256([]byte(strings.Join(components, inboundIDSeparator)))

	return hex.EncodeToString(sum[:])[:inboundIDLength]
}
