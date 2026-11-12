package ping

import (
	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the ping Fx module: the system:ping heartbeat ticker. The
// ticker runnable is registered only when withRun is true. Inside this
// module the app-wide settings store is decorated down to the ping
// namespace, so the settings facade only ever sees "ping.*" keys; Config is
// provided by the config module mapping (internal/config/module.go) from the
// PING__* env keys.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("ping"),
		fx.Decorate(func(store settings.Store) settings.Store {
			return settings.Scoped(store, "ping")
		}),
		fx.Provide(NewSettings, fx.Private),
		// The settings API reaches the ping settings only through the
		// grouped Section; the facade itself stays module-internal.
		fx.Provide(fx.Annotate(
			func(pingSettings *Settings) settings.Section { return pingSettings },
			fx.ResultTags(`group:"settings-sections"`),
		)),
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
