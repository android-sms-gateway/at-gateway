package at_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	atp "github.com/android-sms-gateway/at-gateway/internal/modem/at"
)

// fakePort implements [io.ReadWriteCloser] for testing.
// Write sends data to writeCh, Read receives from readCh.
type fakePort struct {
	readCh  chan []byte
	writeCh chan []byte
}

func newFakePort() *fakePort {
	return &fakePort{
		readCh:  make(chan []byte, 100),
		writeCh: make(chan []byte, 100),
	}
}

func (p *fakePort) Read(b []byte) (int, error) {
	data, ok := <-p.readCh
	if !ok {
		return 0, io.EOF
	}

	return copy(b, data), nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	buf := make([]byte, len(b))
	copy(buf, b)
	p.writeCh <- buf

	return len(b), nil
}

func (p *fakePort) Close() error {
	close(p.readCh)

	return nil
}

func TestStop_DuringIdleRead(t *testing.T) {
	p := newFakePort()
	at := atp.NewAT(atp.Config{}, p)
	at.Start()

	// Give readLoop time to block in ReadByte on the idle port.
	time.Sleep(50 * time.Millisecond)

	at.Stop()
	at.Stop() // repeated Stop must not panic

	select {
	case <-at.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not close after Stop during an idle read")
	}
}

func TestExecRaw_HappyPath(t *testing.T) {
	p := newFakePort()
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	errCh := make(chan error, 1)
	respCh := make(chan *atp.Response, 1)

	go func() {
		resp, err := at.ExecRaw(context.Background(), `AT+CMGS="+1234567890"`, "Hello")
		if err != nil {
			errCh <- err

			return
		}

		respCh <- resp
	}()

	// Read the command
	cmd := <-p.writeCh
	if string(cmd) != `AT+CMGS="+1234567890"`+"\r\n" {
		t.Fatalf("unexpected command: %q", string(cmd))
	}

	// Send prompt (real modems send "> " without a trailing newline)
	p.readCh <- []byte("> ")

	// Read payload + Ctrl-Z
	payload := <-p.writeCh
	if string(payload) != "Hello\x1a" {
		t.Fatalf("unexpected payload: %q", string(payload))
	}

	// Send response
	p.readCh <- []byte("+CMGS: 42\r\nOK\r\n")

	select {
	case resp := <-respCh:
		if !resp.OK {
			t.Fatal("expected OK")
		}

		if resp.Ref != 42 {
			t.Fatalf("expected Ref=42, got %d", resp.Ref)
		}
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ExecRaw")
	}
}

func TestExecRaw_PromptWithCRLF(t *testing.T) {
	p := newFakePort()
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	errCh := make(chan error, 1)
	respCh := make(chan *atp.Response, 1)

	go func() {
		resp, err := at.ExecRaw(context.Background(), `AT+CMGS="+1234567890"`, "Hello")
		if err != nil {
			errCh <- err

			return
		}

		respCh <- resp
	}()

	// Read the command
	<-p.writeCh

	// Send prompt with a trailing newline, as some modems do
	p.readCh <- []byte("\r\n> \r\n")

	// Read payload + Ctrl-Z
	payload := <-p.writeCh
	if string(payload) != "Hello\x1a" {
		t.Fatalf("unexpected payload: %q", string(payload))
	}

	// Send response
	p.readCh <- []byte("+CMGS: 7\r\nOK\r\n")

	select {
	case resp := <-respCh:
		if !resp.OK {
			t.Fatal("expected OK")
		}

		if resp.Ref != 7 {
			t.Fatalf("expected Ref=7, got %d", resp.Ref)
		}
	case err := <-errCh:
		t.Fatalf("unexpected error: %v", err)
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ExecRaw")
	}
}

func TestExecRaw_NoPrompt_ContextTimeout(t *testing.T) {
	p := newFakePort()
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := at.ExecRaw(ctx, `AT+CMGS="+1234567890"`, "Hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	if !errors.Is(err, atp.ErrTimeout) {
		t.Fatalf("expected ErrModemTimeout, got %v", err)
	}
}

func TestExecRaw_CtxCancelledDuringPrompt(t *testing.T) {
	p := newFakePort()
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)

	go func() {
		_, err := at.ExecRaw(ctx, `AT+CMGS="+1234567890"`, "Hello")
		errCh <- err
	}()

	// Read the command
	<-p.writeCh

	// Cancel the context before we send any prompt
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error, got nil")
		}

		if !errors.Is(err, atp.ErrTimeout) {
			t.Fatalf("expected ErrModemTimeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for ExecRaw to return after cancel")
	}
}

// func TestSend_HappyPath(t *testing.T) {
// 	p := newFakePort()
// 	at := atp.NewAT(p)
// 	at.Start()
// 	defer at.Stop()

// 	commands := at.NewCommands(at, at.CommandsConfig{
// 		CommandTimeout: time.Second,
// 	})

// 	errCh := make(chan error, 1)
// 	refCh := make(chan int, 1)

// 	go func() {
// 		ref, err := commands.Send(context.Background(), "+1234567890", "Hello")
// 		if err != nil {
// 			errCh <- err

// 			return
// 		}

// 		refCh <- ref
// 	}()

// 	// Read the command
// 	cmd := <-p.writeCh
// 	if string(cmd) != `AT+CMGS="+1234567890"`+"\r\n" {
// 		t.Fatalf("unexpected command: %q", string(cmd))
// 	}

// 	// Send prompt (real modems send "> " without a trailing newline)
// 	p.readCh <- []byte("> ")

// 	// Read payload
// 	payload := <-p.writeCh
// 	if string(payload) != "Hello\x1a" {
// 		t.Fatalf("unexpected payload: %q", string(payload))
// 	}

// 	// Send response
// 	p.readCh <- []byte("+CMGS: 99\r\nOK\r\n")

// 	select {
// 	case ref := <-refCh:
// 		if ref != 99 {
// 			t.Fatalf("expected ref=99, got %d", ref)
// 		}
// 	case err := <-errCh:
// 		t.Fatalf("unexpected error: %v", err)
// 	case <-time.After(time.Second):
// 		t.Fatal("timeout waiting for Send")
// 	}
// }

// func TestSend_CommandFailed(t *testing.T) {
// 	p := newFakePort()
// 	at := at.NewAT(p)
// 	at.Start()
// 	defer at.Stop()

// 	commands := at.NewCommands(at, at.CommandsConfig{
// 		CommandTimeout: time.Second,
// 	})

// 	errCh := make(chan error, 1)

// 	go func() {
// 		_, err := commands.Send(context.Background(), "+1234567890", "Hello")
// 		errCh <- err
// 	}()

// 	// Read command
// 	<-p.writeCh

// 	// Send prompt (real modems send "> " without a trailing newline)
// 	p.readCh <- []byte("> ")

// 	// Read payload
// 	<-p.writeCh

// 	// Send error response
// 	p.readCh <- []byte("+CMS ERROR: 500\r\n")

// 	select {
// 	case err := <-errCh:
// 		if err == nil {
// 			t.Fatal("expected error, got nil")
// 		}
// 	case <-time.After(time.Second):
// 		t.Fatal("timeout waiting for Send")
// 	}
// }
