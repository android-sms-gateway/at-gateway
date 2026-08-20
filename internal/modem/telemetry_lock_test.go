//nolint:testpackage // in-package tests lock Commands semantics against the library engine.
package modem

// RESTORED-LOCK SUITE (Phase 2): the archived telemetry locks from the
// Phase-1 behavior-lock commit (0aac69e), restored with the methods. Transport
// adapted from the legacy engine (at.NewAT + Exec) to the warthog618 library
// (at.New + Command); semantics locked by these tests are unchanged (per-field
// partial-error, swallow errors, stale-response baseline, parse fixtures).

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Fixture values shared by the telemetry locks. Consts keep goconst from
// flagging the repeated fixture strings in this file.
const (
	fixtureManufacturer = "SIMCOM_SIM800L"
	fixtureModel        = "SIM800L"
	fixtureIMEI         = "123456789012345"
	fixturePhoneNumber  = "+1234567890"
	fixtureCNUMLine     = "+CNUM: \"\",\"+1234567890\",129"
	fixtureICCID        = "89860001020304050607"
	fixtureCarrier      = "MTS"
	fixtureCOPSLine     = "+COPS: 0,0,\"MTS\",7"
	fixtureCSQLine      = "+CSQ: 12,0"
	fixtureCREGLine     = "+CREG: 0,1"
	fixtureErrorLine    = "ERROR"
)

// TestCommands_GetModemInfo locks the GMI/GMM/GSN happy path.
func TestCommands_GetModemInfo(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireGMI: {fixtureManufacturer, "OK"},
		wireGMM: {fixtureModel, "OK"},
		wireGSN: {fixtureIMEI, "OK"},
	})
	commands := newTestCommands(t, m, 2*time.Second)

	info, err := commands.GetModemInfo(context.Background())
	if err != nil {
		t.Fatalf("GetModemInfo: %v", err)
	}
	if info.Manufacturer != fixtureManufacturer {
		t.Fatalf("Manufacturer = %q", info.Manufacturer)
	}
	if info.Model != fixtureModel {
		t.Fatalf("Model = %q", info.Model)
	}
	if info.IMEI != fixtureIMEI {
		t.Fatalf("IMEI = %q", info.IMEI)
	}
}

// TestCommands_GetModemInfo_PartialOnError locks the per-field error
// semantics for every field: a failing field returns partial info (fields up
// to the failure) plus a wrapped error tagged with the field name
// ("manufacturer:" / "model:" / "IMEI:"); later fields are not queried.
func TestCommands_GetModemInfo_PartialOnError(t *testing.T) {
	tests := []struct {
		name    string
		failKey string
		prefix  string
		manuf   string
		model   string
		imei    string
	}{
		{
			name:    "manufacturer fails first",
			failKey: wireGMI,
			prefix:  "manufacturer:",
			manuf:   "",
			model:   "",
			imei:    "",
		},
		{
			name:    "model fails mid-sequence",
			failKey: wireGMM,
			prefix:  "model:",
			manuf:   fixtureManufacturer,
			model:   "",
			imei:    "",
		},
		{
			name:    "IMEI fails last",
			failKey: wireGSN,
			prefix:  "IMEI:",
			manuf:   fixtureManufacturer,
			model:   fixtureModel,
			imei:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newScriptedModem(map[string][]string{
				wireGMI: {fixtureManufacturer, "OK"},
				wireGMM: {fixtureModel, "OK"},
				wireGSN: {fixtureIMEI, "OK"},
			})
			m.setResponse(tt.failKey, []string{fixtureErrorLine})
			commands := newTestCommands(t, m, 2*time.Second)

			info, err := commands.GetModemInfo(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.HasPrefix(err.Error(), tt.prefix) {
				t.Fatalf("error %q does not carry the field prefix %q", err, tt.prefix)
			}
			if info.Manufacturer != tt.manuf {
				t.Fatalf("Manufacturer = %q, want %q (partial info)", info.Manufacturer, tt.manuf)
			}
			if info.Model != tt.model {
				t.Fatalf("Model = %q, want %q (partial info)", info.Model, tt.model)
			}
			if info.IMEI != tt.imei {
				t.Fatalf("IMEI = %q, want %q (partial info)", info.IMEI, tt.imei)
			}
		})
	}
}

// TestCommands_GetSimInfo locks the CNUM/CCID/COPS/CSQ/CREG happy path.
func TestCommands_GetSimInfo(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireCNUM: {fixtureCNUMLine, "OK"},
		wireCCID: {fixtureICCID, "OK"},
		wireCOPS: {fixtureCOPSLine, "OK"},
		wireCSQ:  {fixtureCSQLine, "OK"},
		wireCREG: {fixtureCREGLine, "OK"},
	})
	commands := newTestCommands(t, m, 2*time.Second)

	sim, err := commands.GetSimInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSimInfo: %v", err)
	}
	if sim.PhoneNumber != fixturePhoneNumber {
		t.Fatalf("PhoneNumber = %q", sim.PhoneNumber)
	}
	if sim.ICCID != fixtureICCID {
		t.Fatalf("ICCID = %q", sim.ICCID)
	}
	if sim.Carrier != fixtureCarrier {
		t.Fatalf("Carrier = %q", sim.Carrier)
	}
	if !sim.NetworkRegistered {
		t.Fatal("NetworkRegistered = false, want true")
	}
	if sim.SignalQuality != 12 || sim.SignalPercent != 38 {
		t.Fatalf("signal = %d/%d, want 12/38", sim.SignalQuality, sim.SignalPercent)
	}
}

// TestCommands_GetSimInfo_SwallowErrors locks the swallow semantics: failed
// queries yield empty fields and no error.
func TestCommands_GetSimInfo_SwallowErrors(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireCNUM: {fixtureErrorLine}})
	m.defaultOK = true
	commands := newTestCommands(t, m, 2*time.Second)

	sim, err := commands.GetSimInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSimInfo: %v", err)
	}
	if sim.PhoneNumber != "" || sim.ICCID != "" || sim.Carrier != "" ||
		sim.NetworkRegistered || sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf("sim = %+v, want all zeroed", sim)
	}
}

// TestCommands_StaleResponseBaseline locks the stale-response behavior: a
// timed-out query leaves stale lines, yet the next command still returns its
// own clean response (lazy drain barrier on the library engine; legacy
// per-Exec drain pre-swap).
//
// The delayed CNUM fixture carries NO trailing OK: the stale "+CNUM:" info
// line alone must be absorbed by the lazy drain, whose own OK then terminates
// the drain response. A trailing OK in the fixture would race the drain's
// return (the harness bare-AT->OK response is written by the modem right
// after the stale line) and could leak into the next command's response,
// shifting the FIFO (the plan's barrier timing note). The OK-less fixture
// makes the drain absorption deterministic.
func TestCommands_StaleResponseBaseline(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireGMI:  {fixtureManufacturer, "OK"},
		wireCNUM: {fixtureCNUMLine},
	})
	m.defaultOK = true
	m.delay(wireCNUM, 500*time.Millisecond)
	commands := newTestCommands(t, m, 200*time.Millisecond)

	sim, err := commands.GetSimInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSimInfo: %v", err)
	}
	if sim.PhoneNumber != "" {
		t.Fatalf("PhoneNumber = %q, want empty (CNUM timed out)", sim.PhoneNumber)
	}

	info, err := commands.GetModemInfo(context.Background())
	if err != nil {
		t.Fatalf("GetModemInfo after timeout: %v", err)
	}
	if info.Manufacturer != fixtureManufacturer {
		t.Fatalf("Manufacturer = %q, stale lines corrupted the next command", info.Manufacturer)
	}
}
