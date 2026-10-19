package modem_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"go.uber.org/zap"
)

// TestMain warms the GSM-7 charset tables on this single goroutine before any
// test runs: modem.NewService calls WarmGSMCharsets, the upstream library
// builds those tables lazily without synchronization, and the parallel tests
// in this package construct services concurrently.
func TestMain(m *testing.M) {
	modem.WarmGSMCharsets()
	os.Exit(m.Run())
}

// Drop reasons, mirrored from the production labels so a rename breaks the
// test instead of silently detaching the metric.
const (
	dropMalformedHead    = "malformed_head"
	dropNotConnected     = "not_connected"
	dropHandlerSaturated = "handler_saturated"
	dropReadFailed       = "read_failed"
	dropDecodeFailed     = "decode_failed"
	dropChannelFull      = "channel_full"
	dropChannelFullSpent = "channel_full_exhausted"
	dropHandlerPanic     = "handler_panic"
)

// cmgdOp is the <op> label the store-deletion failure counter carries.
const cmgdOp = "cmgd"

// inboundChannelCapacity mirrors the pinned consumer-channel capacity, so a
// test that fills it does not hard-code a bare 64.
const inboundChannelCapacity = 64

// storeModem scripts the whole boot sequence plus the store commands:
//
//   - every index serves the configured default PDU;
//   - the first deleteFailures +CMGD attempts are rejected with +CMS ERROR, so
//     the retry backoff parks a handler in its slot for the test to observe;
//   - every issued +CMGD records its index, so a test can prove WHICH entries
//     were deleted and which were left readable in the modem store.
type storeModem struct {
	defaultPDU string
	// stores maps a store NAME (ME, SM, MT) to the entries AT+CMGL=4 reports
	// for it, because the preferred storage selects which store a listing
	// answers for. A successfully deleted index is REMOVED from its store, so
	// each store shrinks exactly like a real modem's and a repeated sweep does
	// not re-read an entry it already delivered.
	stores map[string][]int
	// listErrors names the stores AT+CMGL=4 fails for, so a test can pin that
	// an unreadable store does not blind the sweep to the others.
	listErrors map[string]bool

	mu             sync.Mutex
	selected       string
	reads          int
	deletes        int
	lists          int
	deleteFailures int
	deletedIndexes []int
	// deletedStores is the preferred store AT+CMGD ran against, zipped with
	// deletedIndexes: a delete is only "for index N" evidence once the STORE it
	// was issued against is known too.
	deletedStores []string
	listedStores  []string
}

func (m *storeModem) respond(w string) [][]byte {
	// Handled BEFORE initResponder, which answers +CPMS with a bare OK: this
	// modem must record WHICH store became preferred so a listing, read or
	// delete acts on that store's own entries.
	if mem, found := strings.CutPrefix(w, "AT+CPMS="); found {
		m.mu.Lock()
		m.selected = strings.Trim(strings.TrimSpace(mem), `"`)
		m.mu.Unlock()

		return [][]byte{okResponse("OK")}
	}
	if resps := initResponder(w); resps != nil {
		return resps
	}
	if resps := simResponder(
		`+CNUM: "MegaFon","+79991234567",145`,
		"+CCID: 25001123456789012345",
		`+COPS: 0,0,"MegaFon",7`,
		"+CSQ: 22,99",
		"+CREG: 0,1",
	)(w); resps != nil {
		return resps
	}
	switch {
	case strings.HasPrefix(w, "AT+GMI"):
		return [][]byte{okResponse("MegaFon"), okResponse("OK")}
	case strings.HasPrefix(w, "AT+GMM"):
		return [][]byte{okResponse("MS2131"), okResponse("OK")}
	case strings.HasPrefix(w, "AT+GSN"):
		return [][]byte{okResponse("352093001111111"), okResponse("OK")}
	case strings.HasPrefix(w, "AT+CMGR="):
		return m.readStore(w)
	case strings.HasPrefix(w, "AT+CMGD="):
		return m.deleteStore(w)
	case strings.HasPrefix(w, "AT+CMGL="):
		return m.listStore()
	}

	return nil
}

// listStore answers AT+CMGL=4 with one "+CMGL: <index>,...,<length>" header and
// its PDU body per retained index OF THE PREFERRED STORE.
func (m *storeModem) listStore() [][]byte {
	m.mu.Lock()
	m.lists++
	m.listedStores = append(m.listedStores, m.selected)
	indices := append([]int(nil), m.stores[m.selected]...)
	failed := m.listErrors[m.selected]
	m.mu.Unlock()

	if failed {
		return [][]byte{okResponse("+CMS ERROR: 321")}
	}

	responses := make([][]byte, 0, len(indices)*2+1)
	for _, index := range indices {
		responses = append(responses,
			okResponse(`+CMGL: `+strconv.Itoa(index)+`,1,"",`+strconv.Itoa(len(m.defaultPDU)/2)),
			okResponse(m.defaultPDU),
		)
	}

	return append(responses, okResponse("OK"))
}

func (m *storeModem) readStore(w string) [][]byte {
	m.mu.Lock()
	m.reads++
	m.mu.Unlock()

	pdu := m.defaultPDU
	index := storeIndex(w)
	if index == 0 {
		return [][]byte{okResponse("+CMS ERROR: 321")}
	}

	return [][]byte{
		okResponse(`+CMGR: 1,"",` + strconv.Itoa(len(pdu)/2)),
		okResponse(pdu),
		okResponse("OK"),
	}
}

func (m *storeModem) deleteStore(w string) [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.deletes++
	index := storeIndex(w)
	m.deletedIndexes = append(m.deletedIndexes, index)
	m.deletedStores = append(m.deletedStores, m.selected)
	if m.deletes <= m.deleteFailures {
		return [][]byte{okResponse("+CMS ERROR: 321")}
	}
	// Only a modem seeded with per-store entries shrinks them; the inbound
	// tests that never list a store leave the map nil.
	if m.stores != nil {
		m.stores[m.selected] = slices.DeleteFunc(m.stores[m.selected], func(i int) bool { return i == index })
	}

	return [][]byte{okResponse("OK")}
}

func (m *storeModem) counters() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.reads, m.deletes
}

// listCount returns how many AT+CMGL=4 listings the modem answered.
func (m *storeModem) listCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.lists
}

// listedFor returns how many AT+CMGL=4 listings the modem answered for the
// named store.
func (m *storeModem) listedFor(store string) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	listed := 0
	for _, name := range m.listedStores {
		if name == store {
			listed++
		}
	}

	return listed
}

// deletedFor reports whether AT+CMGD was ever issued for a store index, i.e.
// whether the entry is gone from the modem store.
func (m *storeModem) deletedFor(index int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return slices.Contains(m.deletedIndexes, index)
}

// deletedInStore reports whether AT+CMGD was issued for index while the named
// store was the preferred one, i.e. whether THAT store's entry is gone.
func (m *storeModem) deletedInStore(store string, index int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, deletedIndex := range m.deletedIndexes {
		if deletedIndex == index && m.deletedStores[i] == store {
			return true
		}
	}

	return false
}

// storeIndex extracts the <index> argument of an AT+CMGR/AT+CMGD write. Both
// commands carry the index ALONE (+CMGR: 3GPP TS 27.005 3.4.2; +CMGD: SIM800
// Series AT Command Manual V1.10 sec 4.2.1). The comma cut stays only so a
// two-argument regression still parses on the wire instead of recording 0.
func storeIndex(w string) int {
	var payload string
	switch {
	case strings.HasPrefix(w, "AT+CMGR="):
		payload = strings.TrimPrefix(w, "AT+CMGR=")
	case strings.HasPrefix(w, "AT+CMGD="):
		payload = strings.TrimPrefix(w, "AT+CMGD=")
	default:
		return 0
	}

	if _, index, found := strings.Cut(payload, ","); found {
		payload = index
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(payload))
	if err != nil {
		return 0
	}

	return parsed
}

// servicePort wraps the shared scripted modem for the Service-level tests.
// Its Close only flips the closed flag: the shared harness closes the response
// channel, which would race with a command write that is still in flight when
// the test tears the modem down. Leaving Read blocked is harmless - Run exits
// on context cancellation.
type servicePort struct {
	*scriptedModem
}

func (p *servicePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true

	return nil
}

// newInboundService starts a modem Service on a scripted port and waits until
// it is ready, so tests can push unsolicited +CMTI heads at it.
func newInboundService(
	t *testing.T,
	config modem.Config,
	scripted *storeModem,
) (*modem.Service, *modem.Metrics, *servicePort) {
	t.Helper()

	return newInboundServiceWithTick(t, config, scripted, 0)
}

// newInboundServiceWithTick is newInboundService with an EXPLICIT period for the
// Run ticker, so a test can drive the lifecycle work the tick performs; a zero
// period keeps the production default.
func newInboundServiceWithTick(
	t *testing.T,
	config modem.Config,
	scripted *storeModem,
	tick time.Duration,
) (*modem.Service, *modem.Metrics, *servicePort) {
	t.Helper()

	metrics := modem.NewTestMetrics()
	scratch := &servicePort{&scriptedModem{
		responses: make(chan []byte, 1024),
		onWrite:   scripted.respond,
		mu:        sync.Mutex{},
		writes:    []string{},
		closed:    false,
	}}

	// The scripted modem reaches ready state, so the service gets a real
	// webhooks dependency: its app:started emission is enqueued there.
	webhooksSvc, _ := newWebhookFixture(t)
	service := modem.NewService(config, zap.NewNop(), metrics, webhooksSvc)
	service.SetPortFactory(func(port.Config) (port.Port, error) {
		return scratch, nil
	})
	if tick > 0 {
		service.SetSignalRefreshIntervalForTest(tick)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.Run(ctx)
	}()
	t.Cleanup(func() {
		waitForCount(t, "every inbound handler to return", func() bool {
			return modem.InboundHandlersInflight(metrics) == 0
		})
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("modem Run did not stop after context cancellation")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for service.State() != modem.StateReady {
		if time.Now().After(deadline) {
			t.Fatalf("modem state = %s, want ready", service.State())
		}
		time.Sleep(time.Millisecond)
	}

	return service, metrics, scratch
}

// pushURC writes unsolicited lines into the scripted modem's read stream, the
// way a modem emits them between commands.
func pushURC(scratch *servicePort, lines ...string) {
	for _, line := range lines {
		scratch.responses <- []byte("\r\n" + line + "\r\n")
	}
}

// waitForCount polls until cond holds or the deadline expires.
func waitForCount(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// receivePart reads one inbound part, failing the test on timeout.
func receivePart(t *testing.T, service *modem.Service) modem.InboundPart {
	t.Helper()

	select {
	case part := <-service.InboundParts():
		return part
	case <-time.After(5 * time.Second):
		t.Fatal("no inbound part arrived within 5s")
	}

	return modem.InboundPart{}
}

// inboundServiceConfig is the config the scripted modem runs with: short
// command timeouts keep a silent command bounded.
func inboundServiceConfig() modem.Config {
	return modem.Config{
		Port:           "/dev/null",
		BaudRate:       115200,
		InitTimeout:    5 * time.Second,
		CommandTimeout: 500 * time.Millisecond,
		Cmgd: modem.CmgdConfig{
			RetryAttempts: 1,
			RetryBackoff:  time.Millisecond,
		},
		Store: modem.StoreConfig{
			Memory:        "",
			SweepInterval: 0,
		},
	}
}

// TestInboundRoute_ReadsDecodesAndDeletes pins the whole +CMTI route: the head
// is parsed, the stored PDU is read, decoded and handed to the consumer, and
// the store entry is deleted afterwards. The CNMI init row stays the frozen
// "+CNMI=2,1,0,1,0" (mt=1: store + emit +CMTI) and +CMT is no longer
// registered, so the head carries no PII at all.
func TestInboundRoute_ReadsDecodesAndDeletes(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)

	pushURC(scratch, `+CMTI: "ME",1`)
	part := receivePart(t, service)

	if part.Sender != "+79990001234" {
		t.Errorf("sender = %q, want +79990001234", part.Sender)
	}
	if part.Content != "hello" {
		t.Errorf("content = %q, want %q", part.Content, "hello")
	}
	if !part.TimestampValid || part.ReceivedAt.IsZero() {
		t.Error("receivedAt is absent, want the decoded TP-SCTS")
	}

	waitForCount(t, "the handler to finish", func() bool {
		_, deletes := scripted.counters()

		return deletes == 1 && modem.InboundHandlersInflight(metrics) == 0
	})
	if reads, _ := scripted.counters(); reads != 1 {
		t.Errorf("+CMGR attempts = %d, want 1", reads)
	}

	for _, reason := range []string{
		dropMalformedHead,
		dropNotConnected,
		dropHandlerSaturated,
		dropReadFailed,
		dropDecodeFailed,
		dropChannelFull,
		dropChannelFullSpent,
		dropHandlerPanic,
	} {
		if got := modem.InboundPartDropped(metrics, reason); got != 0 {
			t.Errorf("dropped reason=%s = %v, want 0", reason, got)
		}
	}
	if got := modem.InboundHandlersInflight(metrics); got != 0 {
		t.Errorf("handlers inflight = %v, want 0 after the handler returned", got)
	}
	if got := modem.StoreCommandFailure(metrics, cmgdOp); got != 0 {
		t.Errorf("store failures op=%s = %v, want 0", cmgdOp, got)
	}

	writes := strings.Join(scratch.getWrites(), "")
	if !strings.Contains(writes, "AT+CNMI=2,1,0,1,0") {
		t.Error("the frozen +CNMI=2,1,0,1,0 init row is gone from the wire")
	}
}

// TestInboundRoute_EmitsEveryPartUnchanged pins that the modem layer hands
// every decoded part over verbatim - multi-part concat (both widths) and
// port-addressed data included - because REASSEMBLY belongs to the consumer.
func TestInboundRoute_EmitsEveryPartUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pdu         string
		wantContent string
		wantPort    uint16
		wantPorted  bool
		wantConcat  *modem.ConcatRef
	}{
		{
			name:        "concat 8-bit part 1",
			pdu:         inboundConcat8Part1PDU,
			wantContent: "abcdefgh",
			wantPort:    0,
			wantPorted:  false,
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 1, Ref: 0x42, RefWidth: 8},
		},
		{
			name:        "concat 16-bit part 1",
			pdu:         inboundConcat16Part1PDU,
			wantContent: "abcdefgh",
			wantPort:    0,
			wantPorted:  false,
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 1, Ref: 0x42, RefWidth: 16},
		},
		{
			name:        "data SMS with 8-bit port",
			pdu:         inboundPort8PDU,
			wantContent: "\xca\xfe",
			wantPort:    22,
			wantPorted:  true,
			wantConcat:  nil,
		},
		{
			name:        "data SMS with 16-bit port",
			pdu:         inboundPort16PDU,
			wantContent: "\xca\xfe\xba\xbe",
			wantPort:    80,
			wantPorted:  true,
			wantConcat:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			scripted := &storeModem{
				defaultPDU:     tt.pdu,
				stores:         nil,
				listErrors:     nil,
				mu:             sync.Mutex{},
				selected:       "",
				reads:          0,
				deletes:        0,
				lists:          0,
				deleteFailures: 0,
				deletedIndexes: []int{},
				deletedStores:  []string{},
				listedStores:   []string{},
			}
			service, _, scratch := newInboundService(t, inboundServiceConfig(), scripted)

			pushURC(scratch, `+CMTI: "ME",1`)
			part := receivePart(t, service)

			if part.Content != tt.wantContent {
				t.Errorf("content = %q, want %q", part.Content, tt.wantContent)
			}
			if part.Ported != tt.wantPorted {
				t.Errorf("ported = %v, want %v", part.Ported, tt.wantPorted)
			}
			if part.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", part.Port, tt.wantPort)
			}
			switch {
			case tt.wantConcat == nil && part.Concat != nil:
				t.Errorf("concat = %+v, want nil", part.Concat)
			case tt.wantConcat != nil && part.Concat == nil:
				t.Errorf("concat = nil, want %+v", tt.wantConcat)
			case tt.wantConcat != nil && *part.Concat != *tt.wantConcat:
				t.Errorf("concat = %+v, want %+v", part.Concat, tt.wantConcat)
			}

			waitForCount(t, "the store delete", func() bool {
				_, deletes := scripted.counters()

				return deletes == 1
			})
		})
	}
}

// TestInboundRoute_UndecodablePDUIsDroppedBeforeTheDelete pins the failure
// order: a stored body that cannot be decoded never reaches the consumer AND
// is not deleted, so the entry stays available for the modem to re-notify.
func TestInboundRoute_UndecodablePDUIsDroppedBeforeTheDelete(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU[:30],
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)

	pushURC(scratch, `+CMTI: "ME",1`)
	waitForCount(t, "the decode-failure drop", func() bool {
		return modem.InboundPartDropped(metrics, dropDecodeFailed) == 1
	})

	if _, deletes := scripted.counters(); deletes != 0 {
		t.Errorf("+CMGD attempts = %d, want 0 for an undecodable body", deletes)
	}
	select {
	case part := <-service.InboundParts():
		t.Fatalf("undecodable part reached the consumer: %+v", part)
	default:
	}
}

// TestInboundRoute_UnreadableStoreEntryIsDropped pins the read-failure branch:
// a modem that rejects +CMGR leaves the consumer untouched and counts the
// read failure.
func TestInboundRoute_UnreadableStoreEntryIsDropped(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)

	// +CMGR=<index> with index 0 makes the scripted modem answer +CMS ERROR.
	pushURC(scratch, `+CMTI: "ME",0`)
	waitForCount(t, "the read-failure drop", func() bool {
		return modem.InboundPartDropped(metrics, dropReadFailed) == 1
	})

	if _, deletes := scripted.counters(); deletes != 0 {
		t.Errorf("+CMGD attempts = %d, want 0 when the read failed", deletes)
	}
	select {
	case part := <-service.InboundParts():
		t.Fatalf("unreadable entry reached the consumer: %+v", part)
	default:
	}
}

// TestStoreSweep_RecoversRetainedEntry pins the retained-entry recovery path:
// an entry that is in the modem store but was NEVER +CMTI-notified - stored
// before this run, or one whose indication was refused - is listed by the sweep
// (AT+CMGL=4), read, delivered and deleted. Without the sweep such an entry sits
// in the store forever, because +CMTI is emitted only for NEW messages.
func TestStoreSweep_RecoversRetainedEntry(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         map[string][]int{"SM": {1}},
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	cfg := inboundServiceConfig()
	cfg.Store = modem.StoreConfig{Memory: "SM", SweepInterval: 25 * time.Millisecond}
	service, metrics, _ := newInboundService(t, cfg, scripted)

	if part := receivePart(t, service); part.Content != "hello" {
		t.Errorf("swept content = %q, want %q", part.Content, "hello")
	}

	waitForCount(t, "the swept entry to be deleted", func() bool {
		_, deletes := scripted.counters()

		return deletes == 1
	})
	if got := modem.StoreSweeps(metrics); got == 0 {
		t.Error("store sweep counter = 0, want at least one non-empty sweep")
	}
}

// TestStoreSweep_DeletesUndecodableEntry pins the sweep's decode-failure policy:
// an entry whose PDU can never decode is DELETED by the sweep so a dead store
// slot cannot accumulate (the +CMTI path deliberately keeps it for the sweep to
// reclaim).
func TestStoreSweep_DeletesUndecodableEntry(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU[:30],
		stores:         map[string][]int{"SM": {1}},
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	cfg := inboundServiceConfig()
	cfg.Store = modem.StoreConfig{Memory: "SM", SweepInterval: 25 * time.Millisecond}
	service, metrics, _ := newInboundService(t, cfg, scripted)

	waitForCount(t, "the swept undecodable entry to be deleted", func() bool {
		_, deletes := scripted.counters()

		return deletes == 1
	})
	if got := modem.InboundPartDropped(metrics, dropDecodeFailed); got == 0 {
		t.Error("decode-failed drop = 0, want the swept entry counted")
	}
	select {
	case part := <-service.InboundParts():
		t.Fatalf("undecodable swept part reached the consumer: %+v", part)
	default:
	}
}

// TestStoreSweep_RecoversEntryFromEveryNotifiedStore pins that the sweep lists
// EVERY store that can retain an entry, not just the one the latest +CMTI
// named. A +CMTI names only the store ITS message landed in, so a sweep bound
// to that store strands an entry retained in any store named EARLIER: the store
// fills and its entry is never re-read or deleted.
func TestStoreSweep_RecoversEntryFromEveryNotifiedStore(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         map[string][]int{"SM": {1}, "ME": {2}},
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	cfg := inboundServiceConfig()
	cfg.Store = modem.StoreConfig{Memory: "SM", SweepInterval: 25 * time.Millisecond}
	service, _, scratch := newInboundService(t, cfg, scripted)

	// Name ME so it joins the tracked stores AFTER SM, but with an index the
	// scripted modem refuses to read (index 0), so the +CMTI path delivers
	// nothing: both parts below can only have come from the SWEEP.
	pushURC(scratch, `+CMTI: "ME",0`)

	for range 2 {
		if part := receivePart(t, service); part.Content != "hello" {
			t.Errorf("swept content = %q, want %q", part.Content, "hello")
		}
	}
	waitForCount(t, "both stores to be swept", func() bool {
		_, deletes := scripted.counters()

		return deletes >= 2
	})

	if !scripted.deletedInStore("SM", 1) {
		t.Error("the entry retained in SM was never deleted from SM")
	}
	if !scripted.deletedInStore("ME", 2) {
		t.Error("the entry retained in ME was never deleted from ME")
	}
	if got := scripted.listedFor("SM"); got == 0 {
		t.Error("AT+CMGL was never issued for SM, want the older store swept")
	}
}

// TestStoreSweep_ListFailureDoesNotBlindOtherStores pins that a store which
// cannot be listed is SKIPPED rather than aborting the pass: one unreadable
// store must not make the retained entries of every other store unrecoverable.
func TestStoreSweep_ListFailureDoesNotBlindOtherStores(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         map[string][]int{"SM": {1}, "ME": {2}},
		listErrors:     map[string]bool{"SM": true},
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	cfg := inboundServiceConfig()
	cfg.Store = modem.StoreConfig{Memory: "SM", SweepInterval: 25 * time.Millisecond}
	service, _, scratch := newInboundService(t, cfg, scripted)

	// Name ME so it joins the tracked stores AFTER SM, but with an index the
	// scripted modem refuses to read (index 0), so the +CMTI path delivers
	// nothing and the SWEEP is the only thing that can recover ME's entry.
	pushURC(scratch, `+CMTI: "ME",0`)

	// SM is listed first and fails, so this only arrives if the pass CONTINUES.
	if part := receivePart(t, service); part.Content != "hello" {
		t.Errorf("swept content = %q, want %q", part.Content, "hello")
	}
	waitForCount(t, "the readable store to be swept", func() bool {
		return scripted.deletedInStore("ME", 2)
	})
	if scripted.deletedInStore("SM", 1) {
		t.Error("the unreadable store's entry was deleted, want it left for the next pass")
	}
}

// TestStoreSweep_DisabledByDefault pins that a zero sweep interval disables the
// sweep entirely: the store is never listed, so a modem that does not answer
// +CMGL is unaffected.
func TestStoreSweep_DisabledByDefault(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         map[string][]int{"SM": {1}},
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	_, metrics, _ := newInboundService(t, inboundServiceConfig(), scripted)

	// Give a (disabled) sweep ample time to have fired if it were active.
	time.Sleep(100 * time.Millisecond)
	if got := scripted.listCount(); got != 0 {
		t.Errorf("AT+CMGL sweeps = %d, want 0 with the sweep disabled", got)
	}
	if got := modem.StoreSweeps(metrics); got != 0 {
		t.Errorf("store sweep counter = %v, want 0 with the sweep disabled", got)
	}
}

// TestInboundRoute_MalformedHeadIsDropped pins the head-parse failure branch.
func TestInboundRoute_MalformedHeadIsDropped(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	_, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)

	pushURC(scratch, `+CMTI: "XX",1`)
	waitForCount(t, "the malformed-head drop", func() bool {
		return modem.InboundPartDropped(metrics, dropMalformedHead) == 1
	})

	if reads, _ := scripted.counters(); reads != 0 {
		t.Errorf("+CMGR attempts = %d, want 0 for an unusable memory name", reads)
	}
}

// TestInboundRoute_HandlerSaturationDropsIndications pins the PRODUCER-SIDE
// bound: with a store read held in flight, the pinned handler semaphore fills
// up and further indications are dropped instead of spawning unbounded
// goroutines. The gauge tracks the occupancy.
func TestInboundRoute_HandlerSaturationDropsIndications(t *testing.T) {
	t.Parallel()

	// Every handler that is admitted parks in its CMGD retry backoff, which is
	// how a real modem behaves when it refuses to delete a store entry.
	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 1000,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	config := inboundServiceConfig()
	config.Cmgd = modem.CmgdConfig{RetryAttempts: 2, RetryBackoff: 2 * time.Second}
	service, metrics, scratch := newInboundService(t, config, scripted)

	if got := service.HandlerSlots(); got != 4 {
		t.Fatalf("handler slots = %d, want the pinned 4", got)
	}

	// Four indications fill every slot; each one then parks in the delete
	// backoff while holding its slot.
	pushURC(scratch, `+CMTI: "ME",1`, `+CMTI: "ME",2`, `+CMTI: "ME",3`, `+CMTI: "ME",4`)
	waitForCount(t, "the semaphore to fill", func() bool {
		return modem.InboundHandlersInflight(metrics) == 4
	})

	// The next indication has no slot and is dropped instead of spawning
	// another goroutine.
	pushURC(scratch, `+CMTI: "ME",5`)
	waitForCount(t, "the saturation drop", func() bool {
		return modem.InboundPartDropped(metrics, dropHandlerSaturated) == 1
	})

	// The read count is only stable once every admitted handler returned:
	// each of the four admitted handlers increments the gauge on admission,
	// reads its store entry exactly ONCE and only then releases the slot, so
	// inflight==0 pins all four reads as complete (checking earlier races the
	// reads of handlers that have not been scheduled yet). The dropped
	// indication never reads at all.
	waitForCount(t, "every handler to return", func() bool {
		return modem.InboundHandlersInflight(metrics) == 0
	})
	if reads, _ := scripted.counters(); reads != 4 {
		t.Errorf("+CMGR attempts = %d, want 4 - the dropped indication never reads", reads)
	}

	// The four admitted parts are still delivered: saturation only drops the
	// indications that had no slot.
	if got := len(service.InboundParts()); got != 4 {
		t.Errorf("buffered parts = %d, want the 4 admitted", got)
	}
}

// fillInboundChannel pushes one indication per store index until the consumer
// channel is saturated, so the NEXT hand-off fails on a full channel alone. The
// indications are fed one at a time (waiting for each store read) so the
// producer-side semaphore never saturates.
func fillInboundChannel(t *testing.T, service *modem.Service, scratch *servicePort, scripted *storeModem) {
	t.Helper()

	for index := 1; index <= inboundChannelCapacity; index++ {
		current := index
		pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(current))
		waitForCount(t, "the store read of indication "+strconv.Itoa(current), func() bool {
			reads, _ := scripted.counters()

			return reads == current
		})
	}

	waitForCount(t, "the consumer channel to fill", func() bool {
		return len(service.InboundParts()) == inboundChannelCapacity
	})
}

// TestInboundRoute_FullConsumerChannelKeepsTheStoreEntry pins the CONSUMER-SIDE
// bound: the hand-off to the consumer is non-blocking, so a full channel fails
// it - but the store entry is NOT deleted, because a deleted entry can never be
// re-notified and the recoverable drop would become permanent loss. The failure
// is counted by reason and the entry stays readable in the modem store.
func TestInboundRoute_FullConsumerChannelKeepsTheStoreEntry(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)
	fillInboundChannel(t, service, scratch, scripted)

	const kept = inboundChannelCapacity + 1
	pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(kept))
	waitForCount(t, "the channel-full drop", func() bool {
		return modem.InboundPartDropped(metrics, dropChannelFull) == 1
	})
	if got := modem.InboundPartDropped(metrics, dropHandlerSaturated); got != 0 {
		t.Errorf("dropped reason=%s = %v, want 0", dropHandlerSaturated, got)
	}
	if got := modem.InboundPartDropped(metrics, dropChannelFullSpent); got != 0 {
		t.Errorf("dropped reason=%s = %v, want 0 while the retry budget lasts", dropChannelFullSpent, got)
	}
	if scripted.deletedFor(kept) {
		t.Errorf("+CMGD was issued for index %d after a full-channel drop: "+
			"the entry must stay readable for a re-notification", kept)
	}

	// The modem re-notifies the SAME index: a second read succeeds, which is
	// only possible because the entry was never deleted.
	pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(kept))
	waitForCount(t, "the re-notification to re-read the kept entry", func() bool {
		reads, _ := scripted.counters()

		return reads == kept+1
	})
	if scripted.deletedFor(kept) {
		t.Errorf("+CMGD was issued for index %d on re-notification", kept)
	}
}

// TestInboundRoute_ChannelFullRetryBudgetIsBounded pins the BOUND of the
// recoverable drop: the entry survives the first two full-channel hand-off
// failures (the pinned inboundChannelFullRetries budget) and is deleted on the
// third, so the retry loop cannot run forever. The final loss is counted apart
// from the retryable drop, which is what an operator alerts on.
func TestInboundRoute_ChannelFullRetryBudgetIsBounded(t *testing.T) {
	t.Parallel()

	scripted := &storeModem{
		defaultPDU:     inboundTextPDU,
		stores:         nil,
		listErrors:     nil,
		mu:             sync.Mutex{},
		selected:       "",
		reads:          0,
		deletes:        0,
		lists:          0,
		deleteFailures: 0,
		deletedIndexes: []int{},
		deletedStores:  []string{},
		listedStores:   []string{},
	}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)
	fillInboundChannel(t, service, scratch, scripted)

	const stuck = inboundChannelCapacity + 7

	// reNotify re-notifies the stuck index; the read proves the entry is still
	// in the modem store.
	reNotify := func(attempt int) {
		t.Helper()

		pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(stuck))
		waitForCount(t, "the store read of retry "+strconv.Itoa(attempt), func() bool {
			reads, _ := scripted.counters()

			return reads == inboundChannelCapacity+attempt
		})
	}

	// The pinned budget is 3 attempts: the first two keep the entry...
	for attempt := 1; attempt <= 2; attempt++ {
		reNotify(attempt)
		waitForCount(t, "the retryable drop of attempt "+strconv.Itoa(attempt), func() bool {
			return modem.InboundPartDropped(metrics, dropChannelFull) == float64(attempt)
		})
		if scripted.deletedFor(stuck) {
			t.Fatalf("+CMGD was issued for index %d at attempt %d, want the entry kept "+
				"while the retry budget lasts", stuck, attempt)
		}
	}

	// ...and the third spends it: the entry is finally deleted and the loss is
	// counted apart from the retryable drop.
	reNotify(3)
	waitForCount(t, "the exhausted drop", func() bool {
		return modem.InboundPartDropped(metrics, dropChannelFullSpent) == 1
	})
	waitForCount(t, "the store delete of the exhausted entry", func() bool {
		return scripted.deletedFor(stuck)
	})

	if got := modem.InboundPartDropped(metrics, dropChannelFull); got != 2 {
		t.Errorf("dropped reason=%s = %v, want 2 (the retries that kept the entry)", dropChannelFull, got)
	}
	if got := modem.InboundPartDropped(metrics, dropChannelFullSpent); got != 1 {
		t.Errorf("dropped reason=%s = %v, want exactly 1 final loss", dropChannelFullSpent, got)
	}
	if got := len(service.InboundParts()); got != inboundChannelCapacity {
		t.Errorf("buffered parts = %d, want the channel capacity %d", got, inboundChannelCapacity)
	}
}

// TestInboundRoute_CMGDRetriesExponentialBackoff pins the retry policy: the
// configured attempt count is honoured, the backoff grows exponentially and a
// persistent failure counts store_command_failures_total{op="cmgd"} - while
// the part has already reached the consumer (the delete runs last).
func TestInboundRoute_CMGDRetriesExponentialBackoff(t *testing.T) {
	t.Parallel()

	t.Run("recovers within the attempts", func(t *testing.T) {
		t.Parallel()

		scripted := &storeModem{
			defaultPDU:     inboundTextPDU,
			stores:         nil,
			listErrors:     nil,
			mu:             sync.Mutex{},
			selected:       "",
			reads:          0,
			deletes:        0,
			lists:          0,
			deleteFailures: 2,
			deletedIndexes: []int{},
			deletedStores:  []string{},
			listedStores:   []string{},
		}
		config := inboundServiceConfig()
		config.Cmgd = modem.CmgdConfig{RetryAttempts: 3, RetryBackoff: 5 * time.Millisecond}
		service, metrics, scratch := newInboundService(t, config, scripted)

		// The clock starts BEFORE the push, not after receivePart returns:
		// the delete backoff runs on the handler's own goroutine, so a test
		// goroutine descheduled between the two would otherwise lose part of
		// the 15ms budget and read the retry back as too short.
		start := time.Now()
		pushURC(scratch, `+CMTI: "ME",1`)
		receivePart(t, service)

		waitForCount(t, "three delete attempts", func() bool {
			_, deletes := scripted.counters()

			return deletes == 3
		})
		waitForCount(t, "the third attempt to settle", func() bool {
			return modem.InboundHandlersInflight(metrics) == 0
		})
		elapsed := time.Since(start)

		// Sleep budget is 5ms + 10ms = 15ms of exponential backoff.
		if elapsed < 15*time.Millisecond {
			t.Errorf("retry elapsed = %s, want at least the 15ms sleep budget", elapsed)
		}
		if got := modem.StoreCommandFailure(metrics, cmgdOp); got != 0 {
			t.Errorf("store failures op=%s = %v, want 0 after recovery", cmgdOp, got)
		}
	})

	t.Run("exhausts the attempts", func(t *testing.T) {
		t.Parallel()

		scripted := &storeModem{
			defaultPDU:     inboundTextPDU,
			stores:         nil,
			listErrors:     nil,
			mu:             sync.Mutex{},
			selected:       "",
			reads:          0,
			deletes:        0,
			lists:          0,
			deleteFailures: 10,
			deletedIndexes: []int{},
			deletedStores:  []string{},
			listedStores:   []string{},
		}
		config := inboundServiceConfig()
		config.Cmgd = modem.CmgdConfig{RetryAttempts: 3, RetryBackoff: 5 * time.Millisecond}
		service, metrics, scratch := newInboundService(t, config, scripted)

		pushURC(scratch, `+CMTI: "ME",1`)
		// The consumer still gets the message: the delete failure must not
		// swallow an already-decoded part.
		if part := receivePart(t, service); part.Content != "hello" {
			t.Errorf("content = %q, want %q", part.Content, "hello")
		}

		waitForCount(t, "the exhausted store failure", func() bool {
			return modem.StoreCommandFailure(metrics, cmgdOp) == 1
		})
		if _, deletes := scripted.counters(); deletes != 3 {
			t.Errorf("+CMGD attempts = %d, want exactly 3", deletes)
		}
	})
}

// TestInboundRoute_NoCommandsIsNotConnected pins the nil-Commands safety: an
// indication delivered after the disconnect (or before Run) logs and returns
// without touching the store or panicking.
func TestInboundRoute_NoCommandsIsNotConnected(t *testing.T) {
	t.Parallel()

	metrics := modem.NewTestMetrics()
	service := modem.NewService(inboundServiceConfig(), zap.NewNop(), metrics, nil)

	for _, head := range []string{`+CMTI: "ME",1`, `+CMTI: "ME",2`} {
		service.HandleCMTIForTest([]string{head})
	}

	if got := modem.InboundPartDropped(metrics, dropNotConnected); got != 2 {
		t.Errorf("dropped reason=%s = %v, want 2", dropNotConnected, got)
	}
	if got := modem.InboundHandlersInflight(metrics); got != 0 {
		t.Errorf("handlers inflight = %v, want 0", got)
	}
	select {
	case part := <-service.InboundParts():
		t.Fatalf("part arrived without a modem: %+v", part)
	default:
	}
}

// TestInboundRoute_EmptyIndicationIsDropped pins the empty-head branch, which
// the library can produce when an indication carries no line at all.
func TestInboundRoute_EmptyIndicationIsDropped(t *testing.T) {
	t.Parallel()

	metrics := modem.NewTestMetrics()
	service := modem.NewService(inboundServiceConfig(), zap.NewNop(), metrics, nil)

	service.HandleCMTIForTest(nil)

	if got := modem.InboundPartDropped(metrics, dropMalformedHead); got != 1 {
		t.Errorf("dropped reason=%s = %v, want 1", dropMalformedHead, got)
	}
}

// TestRecoverInbound_SwallowsHandlerPanic pins the panic guard the +CMTI and
// +CDS handlers run under: a panic on the library-spawned handler goroutine is
// swallowed and counted instead of taking the process down (a test-binary
// crash would fail this test loudly).
func TestRecoverInbound_SwallowsHandlerPanic(t *testing.T) {
	t.Parallel()

	metrics := modem.NewTestMetrics()
	service := modem.NewService(inboundServiceConfig(), zap.NewNop(), metrics, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer service.RecoverInboundForTest()
		panic("malformed PDU blew up")
	}()
	<-done

	if got := modem.InboundPartDropped(metrics, dropHandlerPanic); got != 1 {
		t.Errorf("dropped reason=%s = %v, want 1", dropHandlerPanic, got)
	}
}
