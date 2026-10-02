package modem_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// budgetBase is the fixed clock every budget test starts from: the production
// code takes the observation time as a parameter, so the budget window and the
// cap can be crossed without sleeping.
func budgetBase() time.Time {
	return time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
}

// newBudgetService builds a Service with no modem behind it: the full-channel
// budget is booked by the accessor below, which is the same call the +CMTI
// handler makes, so no indication and no store read are needed.
func newBudgetService(t *testing.T) *modem.Service {
	t.Helper()

	return modem.NewService(modem.Config{}, zap.NewNop(), modem.NewTestMetrics(), nil)
}

// TestChannelFullBudget_ExhaustedEntryIsRemovedAndFreesTheMap pins the
// lifecycle of a budget that spent all of its attempts: the entry is REMOVED
// (its loss is already counted as channel_full_exhausted), so the references
// that exhausted theirs cannot fill the tracking map and push every later
// reference past the cap - which used to make a brand new reference final on
// its FIRST drop, quietly degrading the 3-attempt bound for good entries.
func TestChannelFullBudget_ExhaustedEntryIsRemovedAndFreesTheMap(t *testing.T) {
	service := newBudgetService(t)
	limit := modem.ChannelFullBudgetCap()
	retries := modem.ChannelFullRetries()

	// Fill the tracking map with references that each spend the whole budget:
	// the first retries-1 attempts are retryable, the last one is final.
	for reference := 1; reference <= limit; reference++ {
		for attempt := 1; attempt <= retries; attempt++ {
			retry := service.SpendChannelFullForTest(1, reference, budgetBase())
			if want := attempt < retries; retry != want {
				t.Fatalf("reference %d attempt %d retry = %v, want %v", reference, attempt, retry, want)
			}
		}
	}

	if got := service.ChannelFullBudgetsForTest(); got != 0 {
		t.Fatalf("live budgets = %d, want 0 once every budget is exhausted", got)
	}

	// A brand new reference therefore gets a FULL budget instead of being
	// final on its first drop.
	fresh := limit + 1
	for attempt := 1; attempt <= retries; attempt++ {
		retry := service.SpendChannelFullForTest(1, fresh, budgetBase())
		if want := attempt < retries; retry != want {
			t.Errorf("fresh reference attempt %d retry = %v, want %v", attempt, retry, want)
		}
	}
}

// TestChannelFullBudget_ReusedStoreSlotStartsFreshAfterTheWindow pins the
// STORE-SLOT reuse: (memory, index) identifies a slot, not a message, so a slot
// the modem reuses for a NEW message after the budget window must not hand the
// new message the old message's spent budget. The reference below still has one
// attempt left, so a pure counter would make the new message final on its first
// drop.
func TestChannelFullBudget_ReusedStoreSlotStartsFreshAfterTheWindow(t *testing.T) {
	service := newBudgetService(t)
	retries := modem.ChannelFullRetries()

	const reused = 7
	for attempt := 1; attempt < retries; attempt++ {
		if retry := service.SpendChannelFullForTest(1, reused, budgetBase()); !retry {
			t.Fatalf("attempt %d = final, want a retry while the first message's budget lasts", attempt)
		}
	}

	// The window elapses and the modem stores a DIFFERENT message at the same
	// slot: the new message starts from attempt 1.
	reusedAt := budgetBase().Add(modem.ChannelFullBudgetWindow())
	for attempt := 1; attempt < retries; attempt++ {
		if retry := service.SpendChannelFullForTest(1, reused, reusedAt); !retry {
			t.Fatalf("reused slot attempt %d = final, want a fresh budget after the window", attempt)
		}
	}
	if retry := service.SpendChannelFullForTest(1, reused, reusedAt); retry {
		t.Error("reused slot = retry on the third attempt, want the fresh budget spent at 3")
	}

	// The flip side, so the window cannot reset a budget that is still
	// spending: a re-notification INSIDE the window is the same message, so its
	// third attempt is still final.
	const inside = 8
	for attempt := 1; attempt < retries; attempt++ {
		if retry := service.SpendChannelFullForTest(1, inside, budgetBase()); !retry {
			t.Fatalf("in-window attempt %d = final, want a retry while the budget lasts", attempt)
		}
	}
	if retry := service.SpendChannelFullForTest(1, inside, budgetBase().Add(time.Second)); retry {
		t.Error("in-window third attempt = retry, want the same message's budget spent at 3")
	}
}

// TestChannelFullBudget_CapEvictsTheOldestUnderAFlood pins the memory bound and
// its eviction order: a flood of DISTINCT references never pushes the live entry
// count past the cap, and the cap costs the OLDEST reference its budget instead
// of leaving a new one untracked (which was the order-dependence).
func TestChannelFullBudget_CapEvictsTheOldestUnderAFlood(t *testing.T) {
	service := newBudgetService(t)
	limit := modem.ChannelFullBudgetCap()
	flood := 3 * limit

	for reference := 1; reference <= flood; reference++ {
		now := budgetBase().Add(time.Duration(reference) * time.Second)
		if retry := service.SpendChannelFullForTest(1, reference, now); !retry {
			t.Fatalf("reference %d = final on its first drop, want a full fresh budget", reference)
		}
		if got := service.ChannelFullBudgetsForTest(); got > limit {
			t.Fatalf("live budgets = %d after reference %d, want at most the cap %d", got, reference, limit)
		}
	}

	if got := service.ChannelFullBudgetsForTest(); got != limit {
		t.Errorf("live budgets = %d, want exactly the cap %d", got, limit)
	}
	if _, _, tracked := service.ChannelFullBudgetForTest(1, 1); tracked {
		t.Error("the oldest reference is still tracked, want it evicted at the cap")
	}
	if attempts, _, tracked := service.ChannelFullBudgetForTest(1, flood); !tracked || attempts != 1 {
		t.Errorf("newest reference: tracked = %v attempts = %d, want tracked with 1 attempt", tracked, attempts)
	}
}

// TestChannelFullBudget_WindowElapsedEntryIsPhysicallyRemoved pins that a
// window-elapsed budget is REMOVED at the moment it is detected stale, not merely
// bypassed on lookup: an entry only reachable through some OTHER reference's
// failure must leave the map, otherwise it is logically absent but physically
// present and no real opportunity ever reclaims it.
func TestChannelFullBudget_WindowElapsedEntryIsPhysicallyRemoved(t *testing.T) {
	service := newBudgetService(t)
	base := budgetBase()

	// Several store slots book a budget inside the SAME window; none of them is
	// ever re-referenced below, so only a stale detection can reclaim them.
	const stale = 5
	for reference := 1; reference <= stale; reference++ {
		if retry := service.SpendChannelFullForTest(1, reference, base); !retry {
			t.Fatalf("reference %d = final on its first drop, want a retry", reference)
		}
	}
	if got := service.ChannelFullBudgetsForTest(); got != stale {
		t.Fatalf("live budgets = %d, want %d", got, stale)
	}

	// The window elapses and ONE more failure arrives: the stale entries must be
	// deleted, so the live count drops to the new entry alone.
	const fresh = stale + 1
	afterWindow := base.Add(modem.ChannelFullBudgetWindow() + time.Second)
	if retry := service.SpendChannelFullForTest(1, fresh, afterWindow); !retry {
		t.Fatal("a fresh reference = final on its first drop, want a full budget")
	}
	if got := service.ChannelFullBudgetsForTest(); got != 1 {
		t.Errorf("live budgets = %d, want 1: a window-elapsed entry must be physically removed on detection", got)
	}
	for reference := 1; reference <= stale; reference++ {
		if _, _, tracked := service.ChannelFullBudgetForTest(1, reference); tracked {
			t.Errorf("reference %d is still tracked after its window elapsed", reference)
		}
	}
}

// TestChannelFullBudget_RunTickReclaimsWithoutAnyTraffic pins the INDEPENDENT
// reclamation: the Run ticker drops the window-elapsed budgets even when no
// further inbound traffic ever arrives, so a stale entry cannot sit in the map
// until an unrelated failure happens to come along.
func TestChannelFullBudget_RunTickReclaimsWithoutAnyTraffic(t *testing.T) {
	scripted := &storeModem{defaultPDU: inboundTextPDU}
	service, _, _ := newInboundServiceWithTick(t, inboundServiceConfig(), scripted, 25*time.Millisecond)

	// Book one budget whose window has ALREADY elapsed on the real clock the Run
	// ticker reads, so the entry is stale the moment it is booked.
	const abandoned = 11
	abandonedAt := time.Now().UTC().Add(-modem.ChannelFullBudgetWindow() - time.Minute)
	if retry := service.SpendChannelFullForTest(1, abandoned, abandonedAt); !retry {
		t.Fatal("the abandoned reference = final on its first drop, want a retry")
	}
	if got := service.ChannelFullBudgetsForTest(); got != 1 {
		t.Fatalf("live budgets = %d, want the 1 booked above", got)
	}

	// No indication, no store read, no second failure: only the Run tick can
	// reclaim the entry now.
	waitForCount(t, "the Run tick to reclaim the window-elapsed budget", func() bool {
		return service.ChannelFullBudgetsForTest() == 0
	})
}

// TestChannelFullBudget_CapEvictsExpiredBeforeTheOldestLive pins the eviction
// ORDER at the cap: the expired budgets are reclaimed first, and a live budget is
// only sacrificed when reclaiming them was not enough to make room.
func TestChannelFullBudget_CapEvictsExpiredBeforeTheOldestLive(t *testing.T) {
	service := newBudgetService(t)
	limit := modem.ChannelFullBudgetCap()
	window := modem.ChannelFullBudgetWindow()
	base := budgetBase()
	cohort := limit / 2

	// Fill the cap with two cohorts: the first one a full window old, the second
	// one just inside it, so exactly one of them is expired.
	for reference := 1; reference <= cohort; reference++ {
		service.SpendChannelFullForTest(1, reference, base)
	}
	for offset := range cohort {
		now := base.Add(window - time.Second)
		if retry := service.SpendChannelFullForTest(2, 100+offset, now); !retry {
			t.Fatalf("live reference %d = final on its first drop, want a retry", 100+offset)
		}
	}
	if got := service.ChannelFullBudgetsForTest(); got != limit {
		t.Fatalf("live budgets = %d, want the full cap %d", got, limit)
	}

	// The first cohort's window elapses and a new reference needs room: the
	// expired entries are reclaimed, so no LIVE budget has to be sacrificed.
	const arriving = 900
	if retry := service.SpendChannelFullForTest(3, arriving, base.Add(window+time.Second)); !retry {
		t.Fatal("the arriving reference = final on its first drop, want a retry")
	}
	for reference := 1; reference <= cohort; reference++ {
		if _, _, tracked := service.ChannelFullBudgetForTest(1, reference); tracked {
			t.Errorf("expired reference %d is still tracked, want it reclaimed first", reference)
		}
	}
	for offset := range cohort {
		if _, _, tracked := service.ChannelFullBudgetForTest(2, 100+offset); !tracked {
			t.Errorf("live reference %d was evicted, want the expired cohort reclaimed first", 100+offset)
		}
	}
	if got, want := service.ChannelFullBudgetsForTest(), cohort+1; got != want {
		t.Errorf("live budgets = %d, want %d", got, want)
	}
}

// TestChannelFullBudget_DeliveredPartLeavesNoBudget pins that forgetChannelFull
// is wired on the ACCEPTED hand-off, so a message that reached the consumer
// leaves no entry behind: the budget of its store slot must not outlive it and
// leak into a later message in the same slot.
func TestChannelFullBudget_DeliveredPartLeavesNoBudget(t *testing.T) {
	scripted := &storeModem{defaultPDU: inboundTextPDU}
	service, metrics, scratch := newInboundService(t, inboundServiceConfig(), scripted)
	fillInboundChannel(t, service, scratch, scripted)

	const stuck = inboundChannelCapacity + 3
	pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(stuck))
	waitForCount(t, "the full-channel drop", func() bool {
		return modem.InboundPartDropped(metrics, dropChannelFull) == 1
	})
	if got := service.ChannelFullBudgetsForTest(); got != 1 {
		t.Fatalf("live budgets = %d, want the 1 booked by the full-channel drop", got)
	}

	// The consumer drains the channel, so the re-notified entry is accepted.
	for range inboundChannelCapacity {
		receivePart(t, service)
	}
	waitForCount(t, "the consumer channel to drain", func() bool {
		return len(service.InboundParts()) == 0
	})

	pushURC(scratch, `+CMTI: "ME",`+strconv.Itoa(stuck))
	receivePart(t, service)

	waitForCount(t, "the accepted hand-off to forget the budget", func() bool {
		return service.ChannelFullBudgetsForTest() == 0
	})
}

// TestInboundPartsDroppedHelpNamesTheChannelFullOutcomes pins the Help of the
// drop counter: an operator must be able to tell the RETRYABLE drop from the
// FINAL one, and the final one must name the tracking-map eviction - it is no
// longer only a message that spent all of its own attempts.
func TestInboundPartsDroppedHelpNamesTheChannelFullOutcomes(t *testing.T) {
	help := productionMetricHelp(t, "at_gateway_modem_inbound_parts_dropped_total")

	for _, want := range []string{
		"channel_full leaves the modem store entry readable",
		"channel_full_exhausted",
		"tracking map",
		"deleted",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help %q does not contain %q", help, want)
		}
	}
}

// productionMetricHelp reads the Help string of a metric straight off the
// default registry. NewMetrics registers there and panics on a duplicate, so
// the family is built only while it is still absent: a repeated -count run
// reuses the already-registered one.
func productionMetricHelp(t *testing.T, name string) string {
	t.Helper()

	if help, found := gatheredMetricHelp(name); found {
		return help
	}

	// A labelled vector with no child is absent from the gather output, so the
	// family is materialised with one increment of a REAL drop reason.
	modem.NewMetrics().IncInboundPartsDropped(dropChannelFull)

	help, found := gatheredMetricHelp(name)
	if !found {
		t.Fatalf("metric %s was not registered by NewMetrics", name)
	}

	return help
}

func gatheredMetricHelp(name string) (string, bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", false
	}

	for _, family := range families {
		if family.GetName() == name {
			return family.GetHelp(), true
		}
	}

	return "", false
}
