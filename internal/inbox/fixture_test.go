package inbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// allTypes is the full four-value incoming-message enum. An empty or nil
// ListFilter.Types DISABLES the type filter (owner decision D-10), so a test
// that wants the type filter EXPLICITLY ON while still matching every stored
// type names every type through this ONE helper instead of inlining the
// literal.
func allTypes() []smsgateway.IncomingMessageType {
	return []smsgateway.IncomingMessageType{
		smsgateway.IncomingMessageTypeSMS,
		smsgateway.IncomingMessageTypeDataSMS,
		smsgateway.IncomingMessageTypeMMS,
		smsgateway.IncomingMessageTypeMmsDownloaded,
	}
}

// newInboundDB starts the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database and returns the connection
// the inbound fixture builds the webhook registry on. The SAME connection
// carries the inbox table: the inbox package has its own Repository, so the
// inbound fixture persists its messages through it.
func newInboundDB(t *testing.T) *bun.DB {
	t.Helper()

	var bunDB *bun.DB

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    1,
			MaxIdleConns:    1,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(b *bun.DB) {
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start persistence app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop persistence app: %v", stopErr)
		}
	})

	return bunDB
}
