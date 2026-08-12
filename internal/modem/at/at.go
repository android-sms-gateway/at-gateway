package at

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	atLineBuffer    = 100
	atReadTimeout   = 5 * time.Second
	atCmdTerminator = "\r\n"
)

type Response struct {
	Lines    []string
	OK       bool
	Error    bool
	CMEError string
	CMSError string
	Ref      int // +CMGS: <ref> reference number, 0 if not applicable
}

type AT struct {
	config Config

	port   io.ReadWriteCloser
	reader *bufio.Reader

	lines    chan string
	mu       sync.Mutex
	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewAT(config Config, port io.ReadWriteCloser) *AT {
	return &AT{
		config: config,

		port:   port,
		reader: bufio.NewReader(port),

		lines:    make(chan string, atLineBuffer),
		mu:       sync.Mutex{},
		stopOnce: sync.Once{},
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

func (a *AT) Start() {
	go a.readLoop()
}

func (a *AT) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
		_ = a.port.Close()
	})
}

func (a *AT) Done() <-chan struct{} {
	return a.doneCh
}

func (a *AT) readLoop() {
	defer close(a.doneCh)

	line := make([]byte, 0, atLineBuffer)
	for {
		select {
		case <-a.stopCh:
			return
		default:
		}

		b, err := a.reader.ReadByte()
		if err != nil {
			return
		}

		line = append(line, b)

		switch {
		case b == '\n':
			a.emitLine(line)
			line = line[:0]
		case isPrompt(line):
			a.emitLine(line[len(line)-2:])
			line = line[:0]
		}
	}
}

// isPrompt reports whether the buffered bytes end with the "> " SMS prompt.
// The modem sends the AT+CMGS prompt without a trailing newline, so the read
// loop must terminate the line on the prompt itself.
func isPrompt(line []byte) bool {
	return len(line) >= 2 && line[len(line)-2] == '>' && line[len(line)-1] == ' '
}

func (a *AT) emitLine(line []byte) {
	text := strings.TrimRight(string(line), "\r\n")
	if text == "" {
		return
	}

	select {
	case a.lines <- text:
	case <-a.stopCh:
	}
}

func (a *AT) Exec(ctx context.Context, cmd string) (*Response, error) {
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
		<-writeDone
		return nil, fmt.Errorf("write %q: %w", cmd, ctx.Err())
	}

	resp := &Response{
		Lines:    make([]string, 0),
		OK:       false,
		Error:    false,
		CMEError: "",
		CMSError: "",
		Ref:      0,
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
		return resp, fmt.Errorf("timeout %q: %w", cmd, ErrTimeout)
	}

	if readErr != nil {
		return resp, fmt.Errorf("read %q: %w", cmd, readErr)
	}

	return resp, nil
}

// ExecRaw writes cmd, waits for '> ' prompt, writes payload+0x1a, reads final response.
// Used for two-phase AT commands like AT+CMGS.
func (a *AT) ExecRaw(ctx context.Context, cmd, payload string) (*Response, error) {
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
		<-writeDone
		return nil, fmt.Errorf("write %q: %w", cmd, ctx.Err())
	}

	// Wait for '> ' prompt
	promptDone := make(chan error, 1)
	go func() {
		promptDone <- a.waitPrompt(ctx)
	}()

	select {
	case err := <-promptDone:
		if err != nil {
			return nil, fmt.Errorf("prompt %q: %w", cmd, err)
		}
	case <-ctx.Done():
		<-promptDone

		return nil, fmt.Errorf("prompt %q: %w", cmd, ErrTimeout)
	}

	// Write payload + Ctrl-Z
	writeDone = make(chan error, 1)
	go func() {
		_, err := a.port.Write([]byte(payload + "\x1a"))
		writeDone <- err
	}()

	select {
	case err := <-writeDone:
		if err != nil {
			return nil, fmt.Errorf("write payload %q: %w", cmd, err)
		}
	case <-ctx.Done():
		<-writeDone
		return nil, fmt.Errorf("write payload %q: %w", cmd, ctx.Err())
	}

	resp := &Response{
		Lines:    make([]string, 0),
		OK:       false,
		Error:    false,
		CMEError: "",
		CMSError: "",
		Ref:      0,
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

		return resp, fmt.Errorf("timeout %q: %w", cmd, ErrTimeout)
	}

	if readErr != nil {
		return resp, fmt.Errorf("read %q: %w", cmd, readErr)
	}

	// Parse +CMGS: <ref> from response lines
	for _, line := range resp.Lines {
		if suffix, ok := strings.CutPrefix(line, "+CMGS:"); ok {
			refStr := strings.TrimSpace(suffix)
			if ref, err := strconv.Atoi(refStr); err == nil {
				resp.Ref = ref
			}

			break
		}
	}

	return resp, nil
}

func (a *AT) readTimeout() time.Duration {
	if a.config.Timeout > 0 {
		return a.config.Timeout
	}

	return atReadTimeout
}

func (a *AT) readResponse(ctx context.Context, resp *Response) error {
	deadline := time.After(a.readTimeout())
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
			return ErrTimeout
		case <-deadline:
			return ErrTimeout
		}
	}
}

func (a *AT) waitPrompt(ctx context.Context) error {
	deadline := time.After(a.readTimeout())
	for {
		select {
		case line := <-a.lines:
			if strings.TrimRight(line, " ") == ">" {
				return nil
			}

			if line == "ERROR" {
				return ErrCommandFailed
			}

			if strings.HasPrefix(line, "+CMS ERROR:") {
				return ErrCommandFailed
			}

			if strings.HasPrefix(line, "+CME ERROR:") {
				return ErrCommandFailed
			}
		case <-ctx.Done():
			return ErrTimeout
		case <-deadline:
			return ErrTimeout
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
