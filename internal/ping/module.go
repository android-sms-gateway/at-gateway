package ping

import (
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the ping Fx module: the system:ping heartbeat ticker. The
// ticker runnable is registered only when withRun is true. Config is provided by
// the config module mapping (internal/config/module.go) from the PING__* env
// keys.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("ping"),
		fx.Provide(NewService),
	}

	if withRun {
		opts = append(
			opts,
			fx.Invoke(fxutil.RegisterRunnable[*Service]()),
		)
	}

	return fx.Module("ping", opts...)
}
