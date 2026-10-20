package inbox

import (
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the inbox Fx module: the inbound SMS pipeline (reassembly,
// dedupe, emission). Config is provided by the config module mapping
// (internal/config/module.go) from the INBOX__* env keys; the delivery worker
// is registered only when withRun is true.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("inbox"),

		fx.Provide(NewMetrics, fx.Private),
		fx.Provide(NewService),
	}

	if withRun {
		opts = append(opts, fx.Invoke(fxutil.RegisterRunnable[*Service]()))
	}

	return fx.Module(
		"inbox",
		opts...,
	)
}
