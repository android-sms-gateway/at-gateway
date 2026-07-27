package modem

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	atLineBuffer    = 100
	atReadTimeout   = 5 * time.Second
	atCmdTerminator = "\r\n"
)

type ATResponse struct {
	Lines    []string
	OK       bool
	Error    bool
	CMEError string
	CMSError string
}

type AT struct {
	port   io.ReadWriteCloser
	reader *bufio.Reader

	lines  chan string
	mu     sync.Mutex
	stopCh chan struct{}
	doneCh chan struct{}
}

func NewAT(port io.ReadWriteCloser) *AT {
	return &AT{
		port:   port,
		reader: bufio.NewReader(port),

		lines:  make(chan string, atLineBuffer),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		mu:     sync.Mutex{},
	}
}

func (a *AT) Start() {
	go a.readLoop()
}

func (a *AT) Stop() {
	close(a.stopCh)
}

func (a *AT) Done() <-chan struct{} {
	return a.doneCh
}

func (a *AT) readLoop() {
	defer close(a.doneCh)

	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		line, err := a.reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			select {
			case a.lines <- line:
			case <-a.stopCh:
				return
			}
		}
	}
}

func (a *AT) Exec(ctx context.Context, cmd string) (*ATResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.drain()

	writeDone := make(chan error, 1)
	go func() {
		_, err := a.port.Write([]byte(cmd + atCmdTerminator))
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			return nil, fmt.Errorf("write %q: %w", cmd, err)
		}
	case <-ctx.Done():
		return nil, fmt.Errorf("write %q: %w", cmd, ctx.Err())
	}

	resp := &ATResponse{
		Lines:    make([]string, 0),
		OK:       false,
		Error:    false,
		CMEError: "",
		CMSError: "",
	}
	done := make(chan error, 1)

	go func() {
		done <- a.readResponse(ctx, resp)
	}()

	var readErr error
	select {
	case readErr = <-done:
	case <-ctx.Done():
		<-done
		return resp, fmt.Errorf("timeout %q: %w", cmd, ErrModemTimeout)
	}

	if readErr != nil {
		return resp, fmt.Errorf("read %q: %w", cmd, readErr)
	}

	return resp, nil
}

func (a *AT) readResponse(ctx context.Context, resp *ATResponse) error {
	deadline := time.After(atReadTimeout)
	for {
		select {
		case line := <-a.lines:
			if line == "OK" {
				resp.OK = true
				return nil
			}
			if line == "ERROR" {
				resp.Error = true
				return ErrCommandFailed
			}
			if suffix, ok := strings.CutPrefix(line, "+CMS ERROR:"); ok {
				resp.CMSError = strings.TrimSpace(suffix)
				resp.Error = true
				return ErrCommandFailed
			}
			if suffix, ok := strings.CutPrefix(line, "+CME ERROR:"); ok {
				resp.CMEError = strings.TrimSpace(suffix)
				resp.Error = true
				return ErrCommandFailed
			}
			resp.Lines = append(resp.Lines, line)
		case <-ctx.Done():
			return ErrModemTimeout
		case <-deadline:
			return ErrModemTimeout
		}
	}
}

func (a *AT) drain() {
	for {
		select {
		case <-a.lines:
		default:
			return
		}
	}
}
