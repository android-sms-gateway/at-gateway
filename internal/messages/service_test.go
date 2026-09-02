package messages_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/prometheus/client_golang/prometheus"
	"go.bug.st/serial"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	testPtyBaudRate     = 115200
	testPtyInitTimeout  = 3 * time.Second
	testPtyCmdTimeout   = 2 * time.Second
	testPollInterval    = 10 * time.Millisecond
	testAwaitTimeout    = 5 * time.Second
	testModemRefPhone1  = 11
	testModemRefPhone2  = 22
	testCustomDeviceID  = "custom-device"
	testExplicitExtID   = "client-id-1"
	testNanoIDLength    = 21
	testNoTextError     = "message has no text content"
	testDataPort        = 53739
	testCmsErrorCode    = 500
	testSendDelay       = 300 * time.Millisecond
	testFakeMasterWait  = 5 * time.Second
	testShutdownTimeout = 2 * time.Second
)

// ptyFakeModem drives the master side of a legacy BSD pty pair (/dev/ptypX +
// /dev/ttypX): it answers every AT command with OK, serves the +CPIN? READY
// gate and runs the two-step AT+CMGS flow (">" prompt, then "+CMGS: <ref>"
// after the Ctrl-Z payload terminator). The modem service opens the slave
// itself; the fake acquires the master with retries (the master open fails
// with EAGAIN until a slave is open).
type ptyFakeModem struct {
	master *os.File

	refs   map[string]int
	fails  map[string]error
	delay  time.Duration
	onCMGS func(phone string)

	mu    sync.Mutex
	order []string

	ref     int  // ref of the in-flight send (fake goroutine only)
	payload bool // payload expected after the ">" prompt (fake goroutine only)
}

func newPtyFakeModem() *ptyFakeModem {
	return &ptyFakeModem{
		refs:  map[string]int{},
		fails: map[string]error{},
	}
}

func (f *ptyFakeModem) run(masterPath string) {
	var master *os.File
	for deadline := time.Now().Add(testFakeMasterWait); time.Now().Before(deadline); {
		m, err := os.OpenFile(masterPath, os.O_RDWR, 0)
		if err == nil {
			master = m
			break
		}
		time.Sleep(testPollInterval)
	}
	if master == nil {
		return
	}

	// The pair shares one line discipline and the master open resets it to
	// the defaults (echo + output processing on); re-apply raw mode on the
	// master so the modem wire traffic stays byte-clean.
	term, err := unix.IoctlGetTermios(int(master.Fd()), unix.TIOCGETA)
	if err != nil {
		_ = master.Close()
		return
	}
	term.Lflag &^= unix.ICANON | unix.ECHO | unix.ECHOE | unix.ISIG | unix.IEXTEN
	term.Iflag &^= unix.ICRNL | unix.INLCR | unix.IGNCR | unix.IXON | unix.IXOFF | unix.IXANY | unix.ISTRIP | unix.INPCK | unix.PARMRK | unix.IGNPAR
	term.Oflag &^= unix.OPOST
	term.Cc[unix.VMIN] = 1
	term.Cc[unix.VTIME] = 0
	if ioctlErr := unix.IoctlSetTermios(int(master.Fd()), unix.TIOCSETA, term); ioctlErr != nil {
		_ = master.Close()
		return
	}

	f.mu.Lock()
	f.master = master
	f.mu.Unlock()

	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	lastCR := false
	for {
		n, readErr := master.Read(one)
		if readErr != nil {
			return
		}
		if n == 0 {
			continue
		}
		b := one[0]
		if f.payload {
			buf = append(buf, b)
			if b == 0x1A {
				f.serveSMS()
				buf = buf[:0]
			}
			continue
		}
		switch b {
		case '\r':
			f.respond(string(buf))
			buf = buf[:0]
		case '\n':
			if !lastCR {
				f.respond(string(buf))
				buf = buf[:0]
			}
		default:
			buf = append(buf, b)
		}
		lastCR = b == '\r'
	}
}

// respond answers one received command token. Tokens are split on CR and LF
// (the AT+CMGS command line ends with CR only), so the fake sees every
// command the library writes.
func (f *ptyFakeModem) respond(key string) {
	switch {
	case strings.HasPrefix(key, "AT+CMGS="):
		phone := strings.TrimSuffix(strings.TrimPrefix(key, `AT+CMGS="`), `"`)
		f.mu.Lock()
		f.order = append(f.order, phone)
		f.mu.Unlock()
		if _, fail := f.fails[phone]; fail {
			_, _ = f.master.WriteString("+CMS ERROR: " + itoa(testCmsErrorCode) + "\r\n")
			return
		}
		if f.onCMGS != nil {
			f.onCMGS(phone)
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		f.ref = f.refs[phone]
		_, _ = f.master.WriteString(">\r\n")
		f.payload = true
	case key == "AT+CPIN?":
		_, _ = f.master.WriteString("+CPIN: READY\r\nOK\r\n")
	default:
		_, _ = f.master.WriteString("OK\r\n")
	}
}

// serveSMS answers the payload token (text + Ctrl-Z) with a +CMGS reference.
func (f *ptyFakeModem) serveSMS() {
	_, _ = f.master.WriteString("+CMGS: " + itoa(f.ref) + "\r\nOK\r\n")
	f.payload = false
}

func (f *ptyFakeModem) close() {
	f.mu.Lock()
	master := f.master
	f.mu.Unlock()
	if master != nil {
		_ = master.Close()
	}
}

func (f *ptyFakeModem) receivedOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.order...)
}

// itoa formats an integer for the wire replies.
func itoa(v int) string {
	return strconv.Itoa(v)
}

// freePtySlave finds a legacy pty pair whose slave can be opened (a busy
// pair fails with EAGAIN) and returns the slave path for the modem service.
func freePtySlave(t *testing.T) string {
	t.Helper()
	for _, suffix := range "0123456789abcdef" {
		slavePath := "/dev/ttyp" + string(suffix)
		probe, err := serial.Open(slavePath, &serial.Mode{
			BaudRate: testPtyBaudRate, DataBits: 8, Parity: serial.NoParity, StopBits: serial.OneStopBit,
		})
		if err != nil {
			continue
		}
		_ = probe.Close()
		return slavePath
	}
	t.Skip("no legacy pty pair available on this host")
	return ""
}

// ptyModemMetrics builds a modem Metrics struct with unregistered prometheus
// collectors (promauto would panic on duplicate registration).
func ptyModemMetrics() *modem.Metrics {
	return &modem.Metrics{
		CommandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_modem_commands_total", Help: "AT commands (test)"},
			[]string{"command", "status"},
		),
		CommandDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "test_modem_command_duration_seconds",
				Help:    "AT command duration (test)",
				Buckets: []float64{0.1, 0.5, 1, 2, 5},
			},
		),
		ModemState: prometheus.NewGauge(
			prometheus.GaugeOpts{Name: "test_modem_state", Help: "modem state (test)"},
		),
		SignalQuality: prometheus.NewGauge(
			prometheus.GaugeOpts{Name: "test_modem_signal_quality_percent", Help: "signal quality (test)"},
		),
		ReconnectsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_modem_reconnects_total", Help: "reconnects (test)"},
		),
	}
}

// serviceMetrics builds a messages Metrics struct with unregistered
// prometheus collectors.
func serviceMetrics() *messages.Metrics {
	return &messages.Metrics{
		EnqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_messages_enqueued_total", Help: "messages enqueued (test)"},
		),
		SentTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_messages_sent_total", Help: "messages sent (test)"},
		),
		FailedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_messages_failed_total", Help: "messages failed (test)"},
		),
		CancelledTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_messages_cancelled_total", Help: "messages cancelled (test)"},
		),
	}
}

// newDevicesService boots a real devices service over a temp storage file.
func newDevicesService(t *testing.T) *devices.Service {
	t.Helper()
	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	return devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
}

// newServiceEnv boots the full test environment: the persistence graph, a
// real devices service, a real modem service over a scripted pty fake and the
// messages service with metrics. It returns the service and the repository.
func newServiceEnv(t *testing.T, fake *ptyFakeModem) (*messages.Service, *messages.Repository) {
	t.Helper()
	_, repo := newTestRepo(t)
	devicesSvc := newDevicesService(t)

	slavePath := freePtySlave(t)
	masterPath := strings.Replace(slavePath, "/dev/ttyp", "/dev/ptyp", 1)
	go fake.run(masterPath)
	t.Cleanup(fake.close)

	modemSvc := modem.NewService(modem.Config{
		Port:           slavePath,
		BaudRate:       testPtyBaudRate,
		InitTimeout:    testPtyInitTimeout,
		CommandTimeout: testPtyCmdTimeout,
	}, zap.NewNop(), ptyModemMetrics())

	modemCtx, modemCancel := context.WithCancel(context.Background())
	modemDone := make(chan struct{})
	go func() {
		defer close(modemDone)
		_ = modemSvc.Run(modemCtx)
	}()
	t.Cleanup(func() {
		modemCancel()
		select {
		case <-modemDone:
		case <-time.After(testShutdownTimeout):
		}
	})

	awaitModemReady(t, modemSvc)

	svc := messages.NewServiceWithMetrics(
		messages.Config{PollInterval: testPollInterval},
		repo,
		devicesSvc,
		modemSvc,
		serviceMetrics(),
		zap.NewNop(),
	)
	return svc, repo
}

func awaitModemReady(t *testing.T, modemSvc *modem.Service) {
	t.Helper()
	deadline := time.Now().Add(testAwaitTimeout)
	for modemSvc.State() != modem.StateReady {
		if time.Now().After(deadline) {
			t.Fatalf("modem state = %v, want ready", modemSvc.State())
		}
		time.Sleep(testPollInterval)
	}
}

// runWorker starts the service worker loop and cancels it at cleanup.
func runWorker(t *testing.T, svc *messages.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testShutdownTimeout):
		}
	})
}

// enqueue creates a text message through the service.
func enqueue(t *testing.T, svc *messages.Service, extID string, phones ...string) *messages.Message {
	t.Helper()
	msg, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        extID,
		PhoneNumbers: phones,
	})
	if err != nil {
		t.Fatalf("Enqueue(%q): %v", extID, err)
	}
	return msg
}

// awaitMessageState polls the service until the message reaches the wanted
// state or the deadline expires.
func awaitMessageState(
	t *testing.T,
	svc *messages.Service,
	id string,
	want smsgateway.ProcessingState,
) *messages.Message {
	t.Helper()
	deadline := time.Now().Add(testAwaitTimeout)
	for {
		msg, err := svc.Get(context.Background(), id)
		if err == nil && msg.State == want {
			return msg
		}
		if time.Now().After(deadline) {
			t.Fatalf("message %q state = %v (err %v), want %v", id, stateLabel(msg, err), err, want)
		}
		time.Sleep(testPollInterval)
	}
}

// awaitRecipientState polls the service until the recipient reaches the
// wanted state.
func awaitRecipientState(
	t *testing.T,
	svc *messages.Service,
	id, phone string,
	want smsgateway.ProcessingState,
) *messages.Message {
	t.Helper()
	deadline := time.Now().Add(testAwaitTimeout)
	for {
		msg, err := svc.Get(context.Background(), id)
		if err == nil {
			for _, recipient := range msg.Recipients {
				if recipient.PhoneNumber == phone && recipient.State == want {
					return msg
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("recipient %s of %q did not reach %v (err %v)", phone, id, want, err)
		}
		time.Sleep(testPollInterval)
	}
}

func stateLabel(msg *messages.Message, err error) string {
	if err != nil {
		return "error"
	}
	return string(msg.State)
}

// awaitSendCount waits until the fake modem recorded the wanted number of
// +CMGS commands.
func awaitSendCount(t *testing.T, fake *ptyFakeModem, want int) {
	t.Helper()
	deadline := time.Now().Add(testAwaitTimeout)
	for {
		if got := len(fake.receivedOrder()); got >= want {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("fake modem recorded %d sends, want %d", got, want)
		}
		time.Sleep(testPollInterval)
	}
}

// inputWithText builds a text input with the default phone list.
func inputWithText(text string) messages.MessageInput {
	return messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: text},
		},
		PhoneNumbers: []string{testPhone1},
	}
}

// inputWithPhones builds a text input with the given phone list.
func inputWithPhones(phones ...string) messages.MessageInput {
	return messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		PhoneNumbers: phones,
	}
}

func inputWithData() messages.MessageInput {
	return messages.MessageInput{
		MessageContent: messages.MessageContent{
			DataContent: &smsgateway.DataMessage{Data: "SGVsbG8=", Port: testDataPort},
		},
		PhoneNumbers: []string{testPhone1},
	}
}

func inputWithBoth() messages.MessageInput {
	return messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
			DataContent: &smsgateway.DataMessage{Data: "SGVsbG8=", Port: testDataPort},
		},
		PhoneNumbers: []string{testPhone1},
	}
}

func TestService_Enqueue_Validation(t *testing.T) {
	_, repo := newTestRepo(t)
	svc := messages.NewService(messages.Config{}, repo, nil, nil, nil, zap.NewNop())

	tests := []struct {
		name  string
		input messages.MessageInput
		want  error
	}{
		{name: "non-ascii text", input: inputWithText("привет"), want: messages.ErrInvalidText},
		{name: "empty text", input: inputWithText(""), want: messages.ErrInvalidText},
		{name: "too long text", input: inputWithText(strings.Repeat("a", 161)), want: messages.ErrInvalidText},
		{name: "no phones", input: inputWithPhones(), want: messages.ErrInvalidPhoneNumbers},
		{name: "blank phone", input: inputWithPhones(""), want: messages.ErrInvalidPhoneNumbers},
		{
			name:  "blank phone mixed",
			input: inputWithPhones(testPhone1, "", testPhone2),
			want:  messages.ErrInvalidPhoneNumbers,
		},
		{name: "data content", input: inputWithData(), want: messages.ErrNotSupported},
		{name: "both contents", input: inputWithBoth(), want: messages.ErrInvalidContent},
		{
			name:  "no content",
			input: messages.MessageInput{PhoneNumbers: []string{testPhone1}},
			want:  messages.ErrInvalidContent,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Enqueue(context.Background(), tt.input); !errors.Is(err, tt.want) {
				t.Fatalf("Enqueue error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestService_Enqueue_ExtIDGeneration(t *testing.T) {
	_, repo := newTestRepo(t)
	svc := messages.NewService(messages.Config{}, repo, newDevicesService(t), nil, nil, zap.NewNop())

	msg, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		PhoneNumbers: []string{testPhone1},
	})
	if err != nil {
		t.Fatalf("Enqueue with empty ExtID: %v", err)
	}
	if len(msg.ID) != testNanoIDLength {
		t.Fatalf("generated ID = %q (len %d), want a %d-char nanoid", msg.ID, len(msg.ID), testNanoIDLength)
	}

	stored, err := repo.GetByID(context.Background(), msg.ID)
	if err != nil {
		t.Fatalf("GetByID(%q): %v", msg.ID, err)
	}
	if stored.ID != msg.ID {
		t.Fatalf("stored ID = %q, want the returned ID %q", stored.ID, msg.ID)
	}

	explicit, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        testExplicitExtID,
		PhoneNumbers: []string{testPhone1},
	})
	if err != nil {
		t.Fatalf("Enqueue with explicit ExtID: %v", err)
	}
	if explicit.ID != testExplicitExtID {
		t.Fatalf("explicit ID = %q, want %q", explicit.ID, testExplicitExtID)
	}

	// The same explicit ext_id must not be reusable.
	if _, dupErr := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        testExplicitExtID,
		PhoneNumbers: []string{testPhone2},
	}); !errors.Is(dupErr, messages.ErrAlreadyExists) {
		t.Fatalf("Enqueue duplicate error = %v, want ErrAlreadyExists", dupErr)
	}
}

func TestService_Enqueue_DeviceID(t *testing.T) {
	_, repo := newTestRepo(t)
	devicesSvc := newDevicesService(t)
	svc := messages.NewService(messages.Config{}, repo, devicesSvc, nil, nil, zap.NewNop())

	msg, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		PhoneNumbers: []string{testPhone1},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if msg.DeviceID == "" {
		t.Fatal("DeviceID is empty, want the local device ID")
	}
	if msg.DeviceID != devicesSvc.Get().ID {
		t.Fatalf("DeviceID = %q, want devices service ID %q", msg.DeviceID, devicesSvc.Get().ID)
	}

	override := testCustomDeviceID
	overridden, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		DeviceID:     &override,
		ExtID:        testExplicitExtID,
		PhoneNumbers: []string{testPhone1},
	})
	if err != nil {
		t.Fatalf("Enqueue with override: %v", err)
	}
	if overridden.DeviceID != override {
		t.Fatalf("DeviceID = %q, want override %q", overridden.DeviceID, override)
	}
}

func TestService_Get_List_Cancel(t *testing.T) {
	_, repo := newTestRepo(t)
	devicesSvc := newDevicesService(t)
	svc := messages.NewService(messages.Config{}, repo, devicesSvc, nil, nil, zap.NewNop())

	enqueue(t, svc, "route-a", testPhone1)
	enqueue(t, svc, "route-b", testPhone1)
	enqueue(t, svc, "route-c", testPhone1)

	got, err := svc.Get(context.Background(), "route-a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "route-a" || got.DeviceID != devicesSvc.Get().ID {
		t.Fatalf("Get = %q/%q, want route-a/%q", got.ID, got.DeviceID, devicesSvc.Get().ID)
	}

	if _, missingErr := svc.Get(context.Background(), "missing"); !errors.Is(missingErr, messages.ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", missingErr)
	}

	all, total, err := svc.List(context.Background(), messages.ListFilter{Order: messages.SortAsc})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 || len(all) != 3 {
		t.Fatalf("List total/len = %d/%d, want 3/3", total, len(all))
	}
	for i, want := range []string{"route-a", "route-b", "route-c"} {
		if all[i].ID != want {
			t.Fatalf("List[%d].ID = %q, want %q", i, all[i].ID, want)
		}
		if all[i].DeviceID != devicesSvc.Get().ID {
			t.Fatalf("List[%d].DeviceID = %q, want %q", i, all[i].DeviceID, devicesSvc.Get().ID)
		}
	}

	cancelled, err := svc.Cancel(context.Background(), "route-a")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.State != smsgateway.ProcessingStateCancelled {
		t.Fatalf("cancelled state = %q, want Cancelled", cancelled.State)
	}
	if _, secondErr := svc.Cancel(context.Background(), "route-a"); !errors.Is(secondErr, messages.ErrNotPending) {
		t.Fatalf("second Cancel error = %v, want ErrNotPending", secondErr)
	}
	if _, missingErr := svc.Cancel(context.Background(), "missing"); !errors.Is(missingErr, messages.ErrNotFound) {
		t.Fatalf("Cancel(missing) error = %v, want ErrNotFound", missingErr)
	}
}

func TestDeriveMessageState_Ladder(t *testing.T) {
	tests := []struct {
		name   string
		states []smsgateway.ProcessingState
		want   smsgateway.ProcessingState
	}{
		{
			name:   "all sent",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateSent, smsgateway.ProcessingStateSent},
			want:   smsgateway.ProcessingStateSent,
		},
		{
			name:   "single sent",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateSent},
			want:   smsgateway.ProcessingStateSent,
		},
		{
			name:   "one sent one failed",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateSent, smsgateway.ProcessingStateFailed},
			want:   smsgateway.ProcessingStateSent,
		},
		{
			name:   "one failed one sent",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateFailed, smsgateway.ProcessingStateSent},
			want:   smsgateway.ProcessingStateSent,
		},
		{
			name:   "all failed",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateFailed, smsgateway.ProcessingStateFailed},
			want:   smsgateway.ProcessingStateFailed,
		},
		{
			name:   "single failed",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateFailed},
			want:   smsgateway.ProcessingStateFailed,
		},
		{
			name: "any pending",
			states: []smsgateway.ProcessingState{
				smsgateway.ProcessingStatePending,
				smsgateway.ProcessingStateSent,
				smsgateway.ProcessingStateFailed,
			},
			want: smsgateway.ProcessingStatePending,
		},
		{
			name:   "single pending",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStatePending},
			want:   smsgateway.ProcessingStatePending,
		},
		{
			name: "any cancelled",
			states: []smsgateway.ProcessingState{
				smsgateway.ProcessingStateCancelled,
				smsgateway.ProcessingStateSent,
				smsgateway.ProcessingStateFailed,
			},
			want: smsgateway.ProcessingStateCancelled,
		},
		{
			name:   "single cancelled",
			states: []smsgateway.ProcessingState{smsgateway.ProcessingStateCancelled},
			want:   smsgateway.ProcessingStateCancelled,
		},
		{
			name: "cancelled after pending",
			states: []smsgateway.ProcessingState{
				smsgateway.ProcessingStatePending,
				smsgateway.ProcessingStateCancelled,
			},
			want: smsgateway.ProcessingStatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messages.DeriveMessageState(tt.states); got != tt.want {
				t.Fatalf("DeriveMessageState(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func TestService_Run_SendsInFIFOOrder(t *testing.T) {
	fake := newPtyFakeModem()
	fake.refs = map[string]int{testPhone1: testModemRefPhone1, testPhone2: testModemRefPhone2}
	svc, _ := newServiceEnv(t, fake)
	runWorker(t, svc)

	enqueue(t, svc, "fifo-1", testPhone1)
	enqueue(t, svc, "fifo-2", testPhone2)

	first := awaitMessageState(t, svc, "fifo-1", smsgateway.ProcessingStateSent)
	second := awaitMessageState(t, svc, "fifo-2", smsgateway.ProcessingStateSent)
	awaitSendCount(t, fake, 2)

	if first.Recipients[0].State != smsgateway.ProcessingStateSent ||
		first.Recipients[0].RefID == nil || *first.Recipients[0].RefID != testModemRefPhone1 ||
		first.Recipients[0].Error != nil {
		t.Fatalf("first recipient = %+v, want Sent/ref %d/no error", first.Recipients[0], testModemRefPhone1)
	}
	if second.Recipients[0].State != smsgateway.ProcessingStateSent ||
		second.Recipients[0].RefID == nil || *second.Recipients[0].RefID != testModemRefPhone2 ||
		second.Recipients[0].Error != nil {
		t.Fatalf("second recipient = %+v, want Sent/ref %d/no error", second.Recipients[0], testModemRefPhone2)
	}

	// The message state history holds exactly Pending + Sent: the worker
	// appends only on change.
	for id, msg := range map[string]*messages.Message{"fifo-1": first, "fifo-2": second} {
		if len(msg.States) != 2 {
			t.Fatalf("%s states = %+v, want 2 entries (Pending + Sent)", id, msg.States)
		}
		if _, ok := msg.States[string(smsgateway.ProcessingStateSent)]; !ok {
			t.Fatalf("%s states = %+v, want a Sent entry", id, msg.States)
		}
	}

	// FIFO consumption: the first enqueued message is sent first.
	order := fake.receivedOrder()
	if len(order) != 2 || order[0] != testPhone1 || order[1] != testPhone2 {
		t.Fatalf("send order = %v, want [%s %s]", order, testPhone1, testPhone2)
	}
}

func TestService_Run_PartialRecipientFailure(t *testing.T) {
	fake := newPtyFakeModem()
	fake.refs = map[string]int{testPhone1: testModemRefPhone1}
	fake.fails = map[string]error{testPhone2: errors.New("sim card rejected")}
	svc, _ := newServiceEnv(t, fake)
	runWorker(t, svc)

	enqueue(t, svc, "mixed", testPhone1, testPhone2)

	// Ladder over [Sent, Failed] -> Sent.
	msg := awaitMessageState(t, svc, "mixed", smsgateway.ProcessingStateSent)
	awaitSendCount(t, fake, 1)

	first, second := msg.Recipients[0], msg.Recipients[1]
	if first.State != smsgateway.ProcessingStateSent ||
		first.RefID == nil || *first.RefID != testModemRefPhone1 || first.Error != nil {
		t.Fatalf("first recipient = %+v, want Sent/ref %d/no error", first, testModemRefPhone1)
	}
	if second.State != smsgateway.ProcessingStateFailed || second.RefID != nil || second.Error == nil {
		t.Fatalf("second recipient = %+v, want Failed/error", second)
	}
	if len(msg.States) != 2 {
		t.Fatalf("message states = %+v, want 2 entries (Pending + Sent)", msg.States)
	}
}

func TestService_Run_AllRecipientsFailed(t *testing.T) {
	fake := newPtyFakeModem()
	fake.fails = map[string]error{
		testPhone1: errors.New("modem timeout"),
		testPhone2: errors.New("modem timeout"),
	}
	svc, _ := newServiceEnv(t, fake)
	runWorker(t, svc)

	enqueue(t, svc, "fail-all", testPhone1, testPhone2)

	msg := awaitMessageState(t, svc, "fail-all", smsgateway.ProcessingStateFailed)
	awaitSendCount(t, fake, 2)

	for i, recipient := range msg.Recipients {
		if recipient.State != smsgateway.ProcessingStateFailed || recipient.Error == nil || recipient.RefID != nil {
			t.Fatalf("recipient %d = %+v, want Failed/error/no ref", i, recipient)
		}
	}
	if len(msg.States) != 2 {
		t.Fatalf("message states = %+v, want 2 entries (Pending + Failed)", msg.States)
	}
	if _, has := msg.States[string(smsgateway.ProcessingStateSent)]; has {
		t.Fatalf("message states = %+v, want no Sent entry", msg.States)
	}
}

func TestService_Run_CancelRace(t *testing.T) {
	fake := newPtyFakeModem()
	fake.refs = map[string]int{testPhone1: testModemRefPhone1, testPhone2: testModemRefPhone2}
	cmgsCh := make(chan struct{})
	var once sync.Once
	fake.onCMGS = func(string) {
		once.Do(func() { close(cmgsCh) })
	}
	fake.delay = testSendDelay
	svc, _ := newServiceEnv(t, fake)
	runWorker(t, svc)

	enqueue(t, svc, "race", testPhone1)

	// Cancel while the send is in flight (the fake delays the ">" prompt).
	select {
	case <-cmgsCh:
	case <-time.After(testAwaitTimeout):
		t.Fatal("worker did not start the send")
	}
	if _, err := svc.Cancel(context.Background(), "race"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// The worker finishes the interrupted send but must not overwrite the
	// Cancelled message state.
	awaitRecipientState(t, svc, "race", testPhone1, smsgateway.ProcessingStateSent)

	// The worker keeps running: a later message is still processed.
	enqueue(t, svc, "after-race", testPhone2)
	awaitMessageState(t, svc, "after-race", smsgateway.ProcessingStateSent)

	msg, err := svc.Get(context.Background(), "race")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if msg.State != smsgateway.ProcessingStateCancelled {
		t.Fatalf("message state = %q, want Cancelled (AppendMessageState must not overwrite it)", msg.State)
	}
	if _, has := msg.States[string(smsgateway.ProcessingStateSent)]; has {
		t.Fatalf("message states = %+v, want no Sent entry after cancel", msg.States)
	}
}

func TestService_Run_TextlessMessageFailed(t *testing.T) {
	fake := newPtyFakeModem()
	svc, repo := newServiceEnv(t, fake)
	runWorker(t, svc)

	// A data message created directly through the repository (bypassing
	// Enqueue validation) has no text body; the worker fails it instead of
	// panicking or spinning.
	input := &messages.MessageInput{
		MessageContent: messages.MessageContent{
			DataContent: &smsgateway.DataMessage{Data: "SGVsbG8=", Port: testDataPort},
		},
		ExtID:        "data-1",
		PhoneNumbers: []string{testPhone1},
	}
	if err := repo.Create(context.Background(), input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := awaitMessageState(t, svc, "data-1", smsgateway.ProcessingStateFailed)
	if msg.Recipients[0].State != smsgateway.ProcessingStateFailed ||
		msg.Recipients[0].Error == nil || *msg.Recipients[0].Error != testNoTextError {
		t.Fatalf("recipient = %+v, want Failed with %q", msg.Recipients[0], testNoTextError)
	}
	if got := fake.receivedOrder(); len(got) != 0 {
		t.Fatalf("fake modem received sends %v, want none", got)
	}
}
