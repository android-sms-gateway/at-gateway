package inbox

import "time"

// dedupe.go is the bounded per-webhook TTL dedupe that damps modem
// re-notification: a message id stays suppressed per webhook for 10m
// (inboundDedupeTTL) and at most 1024 ids are remembered
// (inboundDedupeCapacity); the constants stay in types.go.

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
	if ok && now.Sub(entry.seenAt) >= d.ttl {
		// An expired entry serves nothing (see served), so its marks must not
		// outlive it either: keeping them would hide a webhook whose re-enqueue
		// just failed for a whole FRESH TTL, not just the leftover one. The
		// entry keeps its slot in d.order - only the webhook set and the stamp
		// reset, so the id is not appended twice.
		entry = inboundDedupEntry{webhooks: make(map[string]struct{}), seenAt: now}
	}
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
