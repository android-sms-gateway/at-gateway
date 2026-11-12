package webhooks

import (
	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/go-core-fx/fxutil"
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the webhooks Fx module. The delivery worker is registered
// only when withRun is true; T8 supplies the worker implementation. Inside
// this module the app-wide settings store is decorated down to the webhooks
// namespace, so the settings facade only ever sees "webhooks.*" keys.
func Module(withRun bool) fx.Option {
	opts := []fx.Option{
		logger.WithNamedLogger("webhooks"),
		fx.Decorate(func(store settings.Store) settings.Store {
			return settings.Scoped(store, "webhooks")
		}),
		fx.Provide(NewMetrics, fx.Private),
		fx.Provide(NewRepository, fx.Private),
		fx.Provide(NewSettings, fx.Private),
		// The settings API reaches the webhook settings only through the
		// grouped Section; the facade itself stays module-internal.
		fx.Provide(fx.Annotate(
			func(webhooksSettings *Settings) settings.Section { return webhooksSettings },
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

	return fx.Module("webhooks", opts...)
}
