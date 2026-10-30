package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
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

// inboundMessageID is the content-deterministic identity of an assembled
// inbound message: the first 16 hex chars of a SHA-256 over the sender, the
// service-centre stamp (prepended only when timestampValid), the assembled
// content, the destination port and the concatenation reference.
//
// The identity does DOUBLE DUTY. It is the key CONSUMERS deduplicate on,
// because webhook delivery is AT-LEAST-ONCE: a re-notification, a retry and a
// post-restart cycle all carry the same id - and it is the PERSISTED ROW KEY
// (message_id under UNIQUE(device_id, message_id)), which is why
// IncomingMessage.ID equals the webhook messageId (the server contract). When
// the TP-SCTS is absent or implausible the stamp component is OMITTED (not
// zeroed), so a re-notification OBSERVED AT A LATER TIME still hashes to the
// same id: omitting it is precisely why a re-read cannot mint a second row.
//
// ACCEPTED CONSEQUENCE (owner decision D-1, ratified): byte-identical content
// from the same sender without a usable stamp collapses to ONE stored row
// forever WITHIN A DEVICE - the message is listed and replayed once although
// BOTH webhook emissions fire (the dedupe only damps within its TTL). That
// store collapse is counted by at_gateway_inbox_store_conflicts_total, NOT by
// at_gateway_inbox_dedupe_blocked_total, which counts suppressed webhook
// emissions only.
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
