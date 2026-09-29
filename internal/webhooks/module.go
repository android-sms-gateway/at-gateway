package webhooks

import (
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the webhooks Fx module. The delivery worker is registered
// only when withRun is true; T8 supplies the worker implementation.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("webhooks"),
		fx.Provide(NewMetrics, fx.Private),
		fx.Provide(NewRepository, fx.Private),
		fx.Provide(NewService),
	}

	if withRun {
		opts = append(opts, fx.Invoke(fxutil.RegisterRunnable[*Service]()))
	}

	return fx.Module("webhooks", opts...)
}
