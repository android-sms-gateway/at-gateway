package at_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	atp "github.com/android-sms-gateway/at-gateway/internal/modem/at"
)

// blockingWritePort simulates a serial port whose writes can stall: the
// blocked write signals entry on entered, then blocks on release until the
// test unblocks it. If blockOn is 0, every write blocks (subsequent writes
// pass through once release is closed); otherwise only the blockOn-th write
// blocks and the rest complete immediately.
type blockingWritePort struct {
	readCh  chan []byte
	writeCh chan []byte
	entered chan struct{}
	release chan struct{}

	blockOn int
	writes  int

	enteredOnce sync.Once
}

func newBlockingWritePort(blockOn int) *blockingWritePort {
	return &blockingWritePort{
		readCh:      make(chan []byte, 100),
		writeCh:     make(chan []byte, 100),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
		blockOn:     blockOn,
		writes:      0,
		enteredOnce: sync.Once{},
	}
}

func (p *blockingWritePort) Read(b []byte) (int, error) {
	data, ok := <-p.readCh
	if !ok {
		return 0, io.EOF
	}

	return copy(b, data), nil
}

func (p *blockingWritePort) Write(b []byte) (int, error) {
	p.writes++
	buf := make([]byte, len(b))
	copy(buf, b)
	p.writeCh <- buf

	if p.blockOn == 0 || p.writes == p.blockOn {
		p.enteredOnce.Do(func() {
			close(p.entered)
		})
		<-p.release
	}

	return len(b), nil
}

func (p *blockingWritePort) Close() error {
	close(p.readCh)

	return nil
}

func TestExec_CtxCancelledDuringBlockedWrite(t *testing.T) {
	p := newBlockingWritePort(0)
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	ctx1, cancel1 := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := at.Exec(ctx1, "AT")
		errCh <- err
	}()

	<-p.entered // the write goroutine is blocked inside Write

	cmd := <-p.writeCh // the blocked write already pushed its command
	if string(cmd) != "AT\r\n" {
		t.Fatalf("unexpected command: %q", string(cmd))
	}

	cancel1()

	// The lock must not be released while the started write is in flight:
	// Exec may not return yet.
	select {
	case err := <-errCh:
		t.Fatalf("Exec returned with %v while the write was still blocked", err)
	case <-time.After(100 * time.Millisecond):
	}

	// And a following command must not reach the port until the write exits.
	respCh := make(chan error, 1)
	go func() {
		_, err := at.Exec(t.Context(), "AT")
		respCh <- err
	}()

	select {
	case <-p.writeCh:
		t.Fatal("second command reached the port while the first write was still blocked")
	case <-time.After(100 * time.Millisecond):
	}

	close(p.release) // let the blocked write exit

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Exec did not return after the write unblocked")
	}

	// Only now may the next command be written.
	cmd = <-p.writeCh
	if string(cmd) != "AT\r\n" {
		t.Fatalf("unexpected command: %q", string(cmd))
	}

	p.readCh <- []byte("OK\r\n")

	select {
	case err := <-respCh:
		if err != nil {
			t.Fatalf("second Exec failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second Exec did not complete")
	}
}

func TestExecRaw_CtxCancelledDuringBlockedPayloadWrite(t *testing.T) {
	p := newBlockingWritePort(2)
	at := atp.NewAT(atp.Config{}, p)
	at.Start()
	defer at.Stop()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := at.ExecRaw(ctx, `AT+CMGS="+1234567890"`, "Hello")
		errCh <- err
	}()

	cmd := <-p.writeCh
	if string(cmd) != `AT+CMGS="+1234567890"`+"\r\n" {
		t.Fatalf("unexpected command: %q", string(cmd))
	}

	p.readCh <- []byte("> ")

	<-p.entered // the payload write goroutine is blocked inside Write

	cancel()

	// The lock must not be released while the started payload write is in
	// flight: ExecRaw may not return yet.
	select {
	case err := <-errCh:
		t.Fatalf("ExecRaw returned with %v while the payload write was still blocked", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(p.release) // let the blocked payload write exit

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecRaw did not return after the payload write unblocked")
	}
}
