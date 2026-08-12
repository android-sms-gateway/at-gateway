package storage

import (
	"github.com/go-core-fx/logger"
	"go.uber.org/fx"
)

func Module() fx.Option {
	return fx.Module(
		"storage",
		logger.WithNamedLogger("storage"),
		fx.Provide(NewService),
		fx.Invoke(func(lc fx.Lifecycle, s *Service) {
			lc.Append(
				fx.StartStopHook(
					s.Open,
					s.Close,
				),
			)
		}),
	)
}
