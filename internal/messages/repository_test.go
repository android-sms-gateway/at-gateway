package messages_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	testRepoStartTimeout = 10 * time.Second
	testRepoStopTimeout  = 5 * time.Second

	testRepoSingleConn = 1

	testRefID = 42
)

// newTestRepo boots the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database so the embedded migrations
// are applied, enables foreign keys (per-connection in SQLite) and returns
// the bun handle plus the repository under test.
func newTestRepo(t *testing.T) (*bun.DB, *messages.Repository) {
	t.Helper()

	var (
		sqldb *sql.DB
		bundb *bun.DB
	)

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    testRepoSingleConn,
			MaxIdleConns:    testRepoSingleConn,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(sqlDB *sql.DB, bunDB *bun.DB) {
			sqldb = sqlDB
			bundb = bunDB
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), testRepoStartTimeout)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), testRepoStopTimeout)
		defer cancelStop()
		if err := app.Stop(stopCtx); err != nil {
			t.Errorf("stop app: %v", err)
		}
	})

	if _, err := sqldb.ExecContext(context.Background(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	return bundb, messages.NewRepository(bundb)
}

func createMessage(t *testing.T, repo *messages.Repository, extID string, phones ...string) {
	t.Helper()

	input := &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        extID,
		PhoneNumbers: phones,
	}
	if err := repo.Create(context.Background(), input); err != nil {
		t.Fatalf("Create(%q): %v", extID, err)
	}
}

func TestCreateGetByID_RoundTrip(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "msg-1", testPhone1, testPhone2)

	message, err := repo.GetByID(context.Background(), "msg-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if message.ID != "msg-1" {
		t.Fatalf("ID = %q, want msg-1", message.ID)
	}
	if message.State != smsgateway.ProcessingStatePending {
		t.Fatalf("State = %q, want Pending", message.State)
	}
	if message.DeviceID != "" {
		t.Fatalf("DeviceID = %q, want empty (not persisted)", message.DeviceID)
	}
	if message.TextContent == nil || message.TextContent.Text != testText {
		t.Fatalf("TextContent = %+v, want %q", message.TextContent, testText)
	}
	if at, ok := message.States[string(smsgateway.ProcessingStatePending)]; !ok || at.IsZero() {
		t.Fatalf("States[Pending] = %v (ok=%v), want a timestamp", at, ok)
	}

	if len(message.Recipients) != 2 {
		t.Fatalf("Recipients length = %d, want 2 (relation loaded)", len(message.Recipients))
	}
	for i, want := range []string{testPhone1, testPhone2} {
		recipient := message.Recipients[i]
		if recipient.PhoneNumber != want {
			t.Fatalf("Recipients[%d].PhoneNumber = %q, want %q", i, recipient.PhoneNumber, want)
		}
		if recipient.State != smsgateway.ProcessingStatePending {
			t.Fatalf("Recipients[%d].State = %q, want Pending", i, recipient.State)
		}
		if len(recipient.States) != 1 {
			t.Fatalf("Recipients[%d] States length = %d, want 1", i, len(recipient.States))
		}
	}
}

func TestGetByID_ExtIDRouting(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "ext-a", testPhone1)
	createMessage(t, repo, "ext-b", testPhone2)

	for _, id := range []string{"ext-a", "ext-b"} {
		message, err := repo.GetByID(context.Background(), id)
		if err != nil {
			t.Fatalf("GetByID(%q): %v", id, err)
		}
		if message.ID != id {
			t.Fatalf("GetByID(%q).ID = %q, want the same ext_id", id, message.ID)
		}
	}

	if _, err := repo.GetByID(context.Background(), "missing"); !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("GetByID(missing) error = %v, want ErrNotFound", err)
	}
}

func TestGetByID_TTLFromValidUntil(t *testing.T) {
	_, repo := newTestRepo(t)

	validUntil := time.Now().UTC().Add(time.Hour)
	input := &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		MessageOptions: messages.MessageOptions{
			ValidUntil: &validUntil,
		},
		ExtID:        "ttl-1",
		PhoneNumbers: []string{testPhone1},
	}
	if err := repo.Create(context.Background(), input); err != nil {
		t.Fatalf("Create: %v", err)
	}

	message, err := repo.GetByID(context.Background(), "ttl-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if message.TTL == nil {
		t.Fatal("TTL is nil, want remaining seconds")
	}
	if *message.TTL < 3599 || *message.TTL > 3600 {
		t.Fatalf("TTL = %d, want ~3600", *message.TTL)
	}
}

func TestList_FilterCountOrder(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "list-a", testPhone1)
	createMessage(t, repo, "list-b", testPhone1)
	createMessage(t, repo, "list-c", testPhone1)
	createMessage(t, repo, "list-d", testPhone1)

	if err := repo.AppendMessageState(context.Background(), "list-b", smsgateway.ProcessingStateSent); err != nil {
		t.Fatalf("AppendMessageState: %v", err)
	}

	all, total, err := repo.List(context.Background(), messages.ListFilter{Order: messages.SortAsc})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 4 {
		t.Fatalf("total = %d, want 4", total)
	}
	if len(all) != 4 {
		t.Fatalf("results = %d, want 4", len(all))
	}
	for i, want := range []string{"list-a", "list-b", "list-c", "list-d"} {
		if all[i].ID != want {
			t.Fatalf("List asc [%d].ID = %q, want %q", i, all[i].ID, want)
		}
	}

	pendingState := smsgateway.ProcessingStatePending
	pending, pendingTotal, err := repo.List(context.Background(), messages.ListFilter{
		State: &pendingState,
		Order: messages.SortAsc,
	})
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if pendingTotal != 3 || len(pending) != 3 {
		t.Fatalf("pending total/len = %d/%d, want 3/3", pendingTotal, len(pending))
	}
	for _, message := range pending {
		if message.State != smsgateway.ProcessingStatePending {
			t.Fatalf("filtered message %q state = %q, want Pending", message.ID, message.State)
		}
	}

	page, pageTotal, err := repo.List(context.Background(), messages.ListFilter{
		Limit: 2,
		Order: messages.SortAsc,
	})
	if err != nil {
		t.Fatalf("List limit: %v", err)
	}
	if pageTotal != 4 || len(page) != 2 {
		t.Fatalf("limited total/len = %d/%d, want 4/2", pageTotal, len(page))
	}
	if page[0].ID != "list-a" || page[1].ID != "list-b" {
		t.Fatalf("limited page = [%s, %s], want [list-a, list-b]", page[0].ID, page[1].ID)
	}

	nextPage, _, err := repo.List(context.Background(), messages.ListFilter{
		Limit:  2,
		Offset: 2,
		Order:  messages.SortAsc,
	})
	if err != nil {
		t.Fatalf("List offset: %v", err)
	}
	if len(nextPage) != 2 || nextPage[0].ID != "list-c" || nextPage[1].ID != "list-d" {
		t.Fatalf("offset page = %+v, want [list-c, list-d]", nextPage)
	}

	desc, _, err := repo.List(context.Background(), messages.ListFilter{Order: messages.SortDesc})
	if err != nil {
		t.Fatalf("List desc: %v", err)
	}
	for i, want := range []string{"list-d", "list-c", "list-b", "list-a"} {
		if desc[i].ID != want {
			t.Fatalf("List desc [%d].ID = %q, want %q", i, desc[i].ID, want)
		}
	}

	sentState := smsgateway.ProcessingStateSent
	sent, sentTotal, err := repo.List(context.Background(), messages.ListFilter{
		State: &sentState,
		Order: messages.SortAsc,
	})
	if err != nil {
		t.Fatalf("List sent: %v", err)
	}
	if sentTotal != 1 || len(sent) != 1 || sent[0].ID != "list-b" {
		t.Fatalf("sent = %+v (total %d), want [list-b] with total 1", sent, sentTotal)
	}
}

func TestGetNextPending_FIFO(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "fifo-1", testPhone1)
	createMessage(t, repo, "fifo-2", testPhone1)
	createMessage(t, repo, "fifo-3", testPhone1)

	next, err := repo.GetNextPending(context.Background())
	if err != nil {
		t.Fatalf("GetNextPending: %v", err)
	}
	if next.ID != "fifo-1" {
		t.Fatalf("first pending = %q, want fifo-1", next.ID)
	}

	for _, done := range []string{"fifo-1", "fifo-2", "fifo-3"} {
		if appendErr := repo.AppendMessageState(
			context.Background(),
			done,
			smsgateway.ProcessingStateSent,
		); appendErr != nil {
			t.Fatalf("AppendMessageState(%q): %v", done, appendErr)
		}
	}

	if _, emptyErr := repo.GetNextPending(context.Background()); !errors.Is(emptyErr, messages.ErrNotFound) {
		t.Fatalf("GetNextPending on empty queue error = %v, want ErrNotFound", emptyErr)
	}
}

func TestCreate_BatchAtomicity(t *testing.T) {
	bundb, repo := newTestRepo(t)

	input := &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        "dup-pair",
		PhoneNumbers: []string{testPhone1, testPhone1},
	}
	if err := repo.Create(context.Background(), input); !errors.Is(err, messages.ErrDuplicateRecipient) {
		t.Fatalf("Create with duplicated recipient error = %v, want ErrDuplicateRecipient", err)
	}

	if _, err := repo.GetByID(context.Background(), "dup-pair"); !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("GetByID after rollback error = %v, want ErrNotFound", err)
	}

	var messageCount int
	if err := bundb.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM messages").
		Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 0 {
		t.Fatalf("messages after rollback = %d, want 0", messageCount)
	}

	var recipientCount int
	if err := bundb.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM message_recipients").
		Scan(&recipientCount); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if recipientCount != 0 {
		t.Fatalf("recipients after rollback = %d, want 0", recipientCount)
	}
}

func TestCreate_DuplicateExtID(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "dup-1", testPhone1)

	input := &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID:        "dup-1",
		PhoneNumbers: []string{testPhone2},
	}
	if err := repo.Create(context.Background(), input); !errors.Is(err, messages.ErrAlreadyExists) {
		t.Fatalf("Create with duplicate ext_id error = %v, want ErrAlreadyExists", err)
	}

	message, err := repo.GetByID(context.Background(), "dup-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(message.Recipients) != 1 || message.Recipients[0].PhoneNumber != testPhone1 {
		t.Fatalf("recipients = %+v, want only %s (original batch untouched)", message.Recipients, testPhone1)
	}
}

func TestSetRecipientRef(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "ref-msg", testPhone1, testPhone2)

	if err := repo.SetRecipientRef(context.Background(), "ref-msg", testPhone1, testRefID); err != nil {
		t.Fatalf("SetRecipientRef: %v", err)
	}

	message, err := repo.GetByID(context.Background(), "ref-msg")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if message.Recipients[0].RefID == nil || *message.Recipients[0].RefID != testRefID {
		t.Fatalf("Recipients[0].RefID = %v, want %d", message.Recipients[0].RefID, testRefID)
	}
	if message.Recipients[1].RefID != nil {
		t.Fatalf("Recipients[1].RefID = %v, want nil", message.Recipients[1].RefID)
	}

	if setErr := repo.SetRecipientRef(
		context.Background(),
		"ref-msg",
		"unknown-phone",
		testRefID,
	); !errors.Is(
		setErr,
		messages.ErrNotFound,
	) {
		t.Fatalf("SetRecipientRef unknown phone error = %v, want ErrNotFound", setErr)
	}
	if setErr := repo.SetRecipientRef(
		context.Background(),
		"unknown-msg",
		testPhone1,
		testRefID,
	); !errors.Is(
		setErr,
		messages.ErrNotFound,
	) {
		t.Fatalf("SetRecipientRef unknown message error = %v, want ErrNotFound", setErr)
	}
}

func TestUpdateRecipientState(t *testing.T) {
	_, repo := newTestRepo(t)
	createMessage(t, repo, "state-msg", testPhone1, testPhone2)

	refID := 7
	sentErr := "timeout"
	if err := repo.UpdateRecipientState(
		context.Background(), "state-msg", testPhone1,
		smsgateway.ProcessingStateSent, &refID, &sentErr,
	); err != nil {
		t.Fatalf("UpdateRecipientState: %v", err)
	}

	message, err := repo.GetByID(context.Background(), "state-msg")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	first, second := message.Recipients[0], message.Recipients[1]
	if first.PhoneNumber != testPhone1 || second.PhoneNumber != testPhone2 {
		t.Fatalf(
			"recipient order = [%s, %s], want [%s, %s]",
			first.PhoneNumber,
			second.PhoneNumber,
			testPhone1,
			testPhone2,
		)
	}
	if first.State != smsgateway.ProcessingStateSent || first.RefID == nil || *first.RefID != refID ||
		first.Error == nil || *first.Error != sentErr {
		t.Fatalf("first recipient = %+v, want Sent/ref %d/err %q", first, refID, sentErr)
	}
	if len(first.States) != 2 {
		t.Fatalf("first recipient States length = %d, want 2", len(first.States))
	}
	if second.State != smsgateway.ProcessingStatePending || second.Error != nil || len(second.States) != 1 {
		t.Fatalf("second recipient = %+v, want untouched Pending", second)
	}

	// Failed is terminal: a later update must be a no-op.
	if updateErr := repo.UpdateRecipientState(
		context.Background(), "state-msg", testPhone1,
		smsgateway.ProcessingStateFailed, nil, nil,
	); updateErr != nil {
		t.Fatalf("UpdateRecipientState failed: %v", updateErr)
	}
	if updateErr := repo.UpdateRecipientState(
		context.Background(), "state-msg", testPhone1,
		smsgateway.ProcessingStateSent, nil, nil,
	); updateErr != nil {
		t.Fatalf("UpdateRecipientState after failed: %v", updateErr)
	}

	message, err = repo.GetByID(context.Background(), "state-msg")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if message.Recipients[0].State != smsgateway.ProcessingStateFailed {
		t.Fatalf("first recipient State = %q, want Failed (terminal)", message.Recipients[0].State)
	}
	if len(message.Recipients[0].States) != 3 {
		t.Fatalf(
			"first recipient States length = %d, want 3 (failed update was a no-op)",
			len(message.Recipients[0].States),
		)
	}

	if updateErr := repo.UpdateRecipientState(
		context.Background(), "state-msg", "unknown-phone",
		smsgateway.ProcessingStateSent, nil, nil,
	); !errors.Is(updateErr, messages.ErrNotFound) {
		t.Fatalf("UpdateRecipientState unknown phone error = %v, want ErrNotFound", updateErr)
	}
	if updateErr := repo.UpdateRecipientState(
		context.Background(), "unknown-msg", testPhone1,
		smsgateway.ProcessingStateSent, nil, nil,
	); !errors.Is(updateErr, messages.ErrNotFound) {
		t.Fatalf("UpdateRecipientState unknown message error = %v, want ErrNotFound", updateErr)
	}
}

func TestAppendMessageState_SyncInvariant(t *testing.T) {
	bundb, repo := newTestRepo(t)
	createMessage(t, repo, "app-msg", testPhone1)

	if err := repo.AppendMessageState(context.Background(), "app-msg", smsgateway.ProcessingStateSent); err != nil {
		t.Fatalf("AppendMessageState: %v", err)
	}

	var state string
	var lastState string
	if scanErr := bundb.QueryRowContext(
		context.Background(),
		"SELECT state, json_extract(states, '$[#-1].state') FROM messages WHERE ext_id = 'app-msg'",
	).Scan(&state, &lastState); scanErr != nil {
		t.Fatalf("select state invariant: %v", scanErr)
	}
	if state != string(smsgateway.ProcessingStateSent) || lastState != string(smsgateway.ProcessingStateSent) {
		t.Fatalf("state column = %q, last states entry = %q, want both Sent", state, lastState)
	}

	message, err := repo.GetByID(context.Background(), "app-msg")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(message.States) != 2 {
		t.Fatalf("States map length = %d, want 2 (Pending + Sent)", len(message.States))
	}

	// Failed is terminal: appending after Failed must be a no-op.
	if appendErr := repo.AppendMessageState(
		context.Background(),
		"app-msg",
		smsgateway.ProcessingStateFailed,
	); appendErr != nil {
		t.Fatalf("AppendMessageState failed: %v", appendErr)
	}
	if appendErr := repo.AppendMessageState(
		context.Background(),
		"app-msg",
		smsgateway.ProcessingStateSent,
	); appendErr != nil {
		t.Fatalf("AppendMessageState after failed: %v", appendErr)
	}

	if scanErr := bundb.QueryRowContext(
		context.Background(),
		"SELECT state, json_extract(states, '$[#-1].state') FROM messages WHERE ext_id = 'app-msg'",
	).Scan(&state, &lastState); scanErr != nil {
		t.Fatalf("select state invariant: %v", scanErr)
	}
	if state != string(smsgateway.ProcessingStateFailed) || lastState != string(smsgateway.ProcessingStateFailed) {
		t.Fatalf("after failed: state column = %q, last states entry = %q, want both Failed", state, lastState)
	}

	var statesLen int
	if scanErr := bundb.QueryRowContext(
		context.Background(),
		"SELECT json_array_length(states) FROM messages WHERE ext_id = 'app-msg'",
	).Scan(&statesLen); scanErr != nil {
		t.Fatalf("select states length: %v", scanErr)
	}
	if statesLen != 3 {
		t.Fatalf("states length = %d, want 3 (post-Failed append was a no-op)", statesLen)
	}

	if appendErr := repo.AppendMessageState(
		context.Background(),
		"unknown",
		smsgateway.ProcessingStateSent,
	); !errors.Is(
		appendErr,
		messages.ErrNotFound,
	) {
		t.Fatalf("AppendMessageState unknown error = %v, want ErrNotFound", appendErr)
	}
}

func TestCancel(t *testing.T) {
	bundb, repo := newTestRepo(t)
	createMessage(t, repo, "cancel-msg", testPhone1, testPhone2)

	message, err := repo.Cancel(context.Background(), "cancel-msg")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if message.ID != "cancel-msg" || message.State != smsgateway.ProcessingStateCancelled {
		t.Fatalf("cancelled message = %q/%q, want cancel-msg/Cancelled", message.ID, message.State)
	}
	for i, recipient := range message.Recipients {
		if recipient.State != smsgateway.ProcessingStateCancelled {
			t.Fatalf("Recipients[%d].State = %q, want Cancelled", i, recipient.State)
		}
		if len(recipient.States) != 2 {
			t.Fatalf("Recipients[%d] States length = %d, want 2", i, len(recipient.States))
		}
	}

	var state string
	var lastState string
	if scanErr := bundb.QueryRowContext(
		context.Background(),
		"SELECT state, json_extract(states, '$[#-1].state') FROM messages WHERE ext_id = 'cancel-msg'",
	).Scan(&state, &lastState); scanErr != nil {
		t.Fatalf("select cancel invariant: %v", scanErr)
	}
	if state != string(smsgateway.ProcessingStateCancelled) ||
		lastState != string(smsgateway.ProcessingStateCancelled) {
		t.Fatalf("state column = %q, last states entry = %q, want both Cancelled", state, lastState)
	}

	if _, cancelErr := repo.Cancel(context.Background(), "cancel-msg"); !errors.Is(cancelErr, messages.ErrNotPending) {
		t.Fatalf("second Cancel error = %v, want ErrNotPending", cancelErr)
	}
	if _, cancelErr := repo.Cancel(context.Background(), "unknown"); !errors.Is(cancelErr, messages.ErrNotFound) {
		t.Fatalf("Cancel unknown error = %v, want ErrNotFound", cancelErr)
	}
}

func TestMigrations_Schema(t *testing.T) {
	bundb, repo := newTestRepo(t)

	// id INTEGER PRIMARY KEY AUTOINCREMENT.
	var columnType string
	var pk int
	if err := bundb.QueryRowContext(
		context.Background(),
		`SELECT type, pk FROM pragma_table_info('messages') WHERE name = 'id'`,
	).Scan(&columnType, &pk); err != nil {
		t.Fatalf("query id column: %v", err)
	}
	if columnType != "INTEGER" || pk != 1 {
		t.Fatalf("id column = (%s, pk %d), want (INTEGER, pk 1)", columnType, pk)
	}

	var createSQL string
	if err := bundb.QueryRowContext(
		context.Background(),
		"SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'messages'",
	).Scan(&createSQL); err != nil {
		t.Fatalf("query messages schema: %v", err)
	}
	if !strings.Contains(createSQL, "AUTOINCREMENT") {
		t.Fatalf("messages schema = %q, want AUTOINCREMENT", createSQL)
	}

	// ext_id has a unique index.
	var indexName string
	var unique int64
	if err := bundb.QueryRowContext(
		context.Background(),
		`SELECT name, "unique" FROM pragma_index_list('messages') WHERE name = 'idx_messages_ext_id'`,
	).Scan(&indexName, &unique); err != nil {
		t.Fatalf("query ext_id index: %v", err)
	}
	if indexName != "idx_messages_ext_id" || unique != 1 {
		t.Fatalf("ext_id index = (%s, unique %d), want (idx_messages_ext_id, 1)", indexName, unique)
	}

	// message_recipients.message_id -> messages.id ON DELETE CASCADE.
	var refTable string
	var refFrom string
	var onDelete string
	if err := bundb.QueryRowContext(
		context.Background(),
		`SELECT "table", "from", on_delete FROM pragma_foreign_key_list('message_recipients')`,
	).Scan(&refTable, &refFrom, &onDelete); err != nil {
		t.Fatalf("query foreign key: %v", err)
	}
	if refTable != "messages" || refFrom != "message_id" || onDelete != "CASCADE" {
		t.Fatalf("foreign key = (%s, %s, %s), want (messages, message_id, CASCADE)", refTable, refFrom, onDelete)
	}

	// Functional cascade: deleting the message deletes its recipients.
	createMessage(t, repo, "fk-msg", testPhone1)
	if _, err := bundb.ExecContext(context.Background(), "DELETE FROM messages WHERE ext_id = 'fk-msg'"); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	var recipientCount int
	if err := bundb.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM message_recipients").
		Scan(&recipientCount); err != nil {
		t.Fatalf("count recipients: %v", err)
	}
	if recipientCount != 0 {
		t.Fatalf("recipients after cascade = %d, want 0", recipientCount)
	}
}
