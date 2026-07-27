package modem

import (
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/healthfx"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("modem"),

		fx.Provide(NewMetrics, fx.Private),
		fx.Provide(NewService),
		fx.Provide(
			healthfx.AsProvider(NewHealthProvider),
		),
	}

	if withRun {
		opts = append(opts, fx.Invoke(fxutil.RegisterRunnable[*Service]()))
	}

	return fx.Module(
		"modem",
		opts...,
	)
}
