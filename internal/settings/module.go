package settings

import (
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

// Module returns the settings Fx module in two layers: a semantics-free
// key-value store over the SQLite settings table, and the Service that
// aggregates the module sections behind the settings API. Both directions
// ride the "settings-sections" fx group: each module provides its Section
// next to its own scoped-store decoration, and the aggregator only sees
// section names plus raw wire JSON - input and output of the Service are
// smsgateway.DeviceSettings. The root repository stays private to this
// module; the app-wide Store hands out unprefixed access, and a consuming
// module narrows it to its own namespace with an fx.Module-scoped
// fx.Decorate + Scoped (see the ping and webhooks modules), so its
// constructors only ever receive prefixed keys. The section implementations
// are provided by their owning modules.
func Module() fx.Option {
	return fx.Module(
		"settings",
		logger.WithNamedLogger("settings"),
		fx.Provide(NewRepository, fx.Private),
		fx.Provide(func(repo *Repository) Store { return repo }),
		fx.Provide(fx.Annotate(
			NewService,
			fx.ParamTags(`group:"settings-sections"`),
		)),
	)
}
