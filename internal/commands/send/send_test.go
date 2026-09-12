package send_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/commands/send"
	"github.com/urfave/cli/v3"
)

// runSend executes the send command with the given arguments and returns the
// error from Run (usage/parse errors return directly; Action errors flow
// through handleExitCoder, which is overridden so cli's default
// HandleExitCoder - [os.Exit] - never fires in tests).
func runSend(t *testing.T, args []string) error {
	t.Helper()
	cmd := send.Command()
	cmd.ExitErrHandler = func(_ context.Context, _ *cli.Command, _ error) {}

	return cmd.Run(context.Background(), args)
}

// exitCode returns the cli exit code of err, or -1 when err is not an
// ExitCoder.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) {
		t.Fatalf("error %v is not an ExitCoder", err)
	}

	return exitErr.ExitCode()
}

// TestSendCommand_ValidationTooManyParts pins the validation-exit path: text
// that can be encoded (UCS-2 fallback) is accepted, so validation failures are
// limited to structural problems. A text exceeding the part cap exits 1 with
// an actionable message BEFORE any modem wiring.
func TestSendCommand_ValidationTooManyParts(t *testing.T) {
	// 1530 ASCII characters = exactly 10 GSM-7 parts (153 chars each); 1531
	// exceeds the default cap of 10 parts.
	longText := strings.Repeat("a", 1531)
	err := runSend(t, []string{"send", "-p", "+79990001234", "-t", longText})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := exitCode(t, err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "invalid text") {
		t.Fatalf("error %q does not mention invalid text", err)
	}
	if !strings.Contains(err.Error(), "SMS parts long") {
		t.Fatalf("error %q does not describe the part-count overflow", err)
	}
}

// TestSendCommand_MaxPartsFlag pins the --max-parts override: raising the cap
// lets a text that exceeds the config default pass validation and proceed to
// the (deterministically failing) modem port open.
func TestSendCommand_MaxPartsFlag(t *testing.T) {
	t.Setenv("MODEM__PORT", "/dev/this-port-does-not-exist")
	t.Setenv("MODEM__BAUD_RATE", "115200")

	longText := strings.Repeat("a", 1600)
	err := runSend(t, []string{"send", "-p", "+79990001234", "-t", longText, "--max-parts", "11"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := exitCode(t, err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if strings.Contains(err.Error(), "invalid text") {
		t.Fatalf("error %q reports a validation failure for an 11-part text", err)
	}
	if !strings.Contains(err.Error(), "open modem port") {
		t.Fatalf("error %q does not report the port-open failure", err)
	}
}

// TestSendCommand_ValidationEmpty pins the empty-text exit path.
func TestSendCommand_ValidationEmpty(t *testing.T) {
	err := runSend(t, []string{"send", "-p", "+79990001234", "-t", ""})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := exitCode(t, err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "invalid text") {
		t.Fatalf("error %q does not mention invalid text", err)
	}
}

// TestSendCommand_MissingRequiredFlags pins flag parsing: a missing required
// flag fails the run with a usage error naming the flag.
func TestSendCommand_MissingRequiredFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no phone", args: []string{"send", "-t", "hello"}, want: "phone"},
		{name: "no text", args: []string{"send", "-p", "+79990001234"}, want: "text"},
		{name: "both missing", args: []string{"send"}, want: "phone"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runSend(t, tt.args)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), "Required flag") {
				t.Fatalf("error %q is not a required-flags usage error", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention required flag %s", err, tt.want)
			}
		})
	}
}

// TestSendCommand_ValidArgsReachModemWiring pins flag parsing end-to-end for
// a valid ASCII text: the action passes validation and proceeds to open the
// modem port, which fails deterministically against a guaranteed-invalid port
// (no hardware required).
func TestSendCommand_ValidArgsReachModemWiring(t *testing.T) {
	t.Setenv("MODEM__PORT", "/dev/this-port-does-not-exist")
	t.Setenv("MODEM__BAUD_RATE", "115200")

	err := runSend(t, []string{"send", "-p", "+79990001234", "-t", "hello", "--sim", "2", "--timeout", "5s"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := exitCode(t, err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "open modem port") {
		t.Fatalf("error %q does not report the port-open failure", err)
	}
	if strings.Contains(err.Error(), "invalid text") {
		t.Fatalf("error %q reports a validation failure for valid ASCII text", err)
	}
}
