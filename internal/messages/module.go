package messages

import (
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the messages Fx module. Config is provided from package
// defaults until the config-polish wave maps the MESSAGES__* env keys in
// internal/config.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("messages"),

		fx.Provide(NewMetrics, fx.Private),
		fx.Provide(NewRepository),
		fx.Provide(Default),
		fx.Provide(New),
	}

	if withRun {
		opts = append(opts, fx.Invoke(fxutil.RegisterRunnable[*Service]()))
	}

	return fx.Module(
		"messages",
		opts...,
	)
}
