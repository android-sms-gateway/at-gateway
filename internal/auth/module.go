package auth

import (
	"context"

	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

func Module() fx.Option {
	return fx.Module(
		"auth",
		logger.WithNamedLogger("auth"),
		fx.Provide(NewService),
		fx.Invoke(func(lc fx.Lifecycle, svc *Service) {
			lc.Append(fx.Hook{
				OnStart: func(context.Context) error {
					return svc.logBootstrapCredentials()
				},
				OnStop: func(context.Context) error {
					return nil
				},
			})
		}),
	)
}
