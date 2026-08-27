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

// TestSendCommand_ValidationNonASCII pins the validation-exit path: non-ASCII
// text exits 1 with an actionable message BEFORE any config or modem wiring.
func TestSendCommand_ValidationNonASCII(t *testing.T) {
	err := runSend(t, []string{"send", "-p", "+79990001234", "-t", "привет"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if code := exitCode(t, err); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(err.Error(), "invalid text") {
		t.Fatalf("error %q does not mention invalid text", err)
	}
	if !strings.Contains(err.Error(), "U+043F") {
		t.Fatalf("error %q does not describe the offending rune", err)
	}
}

// TestSendCommand_ValidationTooLong pins the over-length exit path.
func TestSendCommand_ValidationTooLong(t *testing.T) {
	longText := strings.Repeat("a", 161)
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
