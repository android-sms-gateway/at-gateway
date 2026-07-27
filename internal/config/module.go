package config

import (
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/go-core-fx/fiberfx"
	"github.com/go-core-fx/fiberfx/openapi"
	"go.uber.org/fx"
)

func Module() fx.Option {
	return fx.Module(
		"config",
		fx.Provide(New, fx.Private),
		fx.Provide(
			func(cfg Config) fiberfx.Config {
				return fiberfx.Config{
					Address:     cfg.HTTP.Address,
					ProxyHeader: cfg.HTTP.ProxyHeader,
					Proxies:     cfg.HTTP.Proxies,
				}
			},
			func(cfg Config) openapi.Config {
				return openapi.Config{
					Enabled:    cfg.HTTP.OpenAPI.Enabled,
					PublicHost: cfg.HTTP.OpenAPI.PublicHost,
					PublicPath: cfg.HTTP.OpenAPI.PublicPath,
				}
			},
		),
		fx.Provide(func(cfg Config) modem.Config {
			return modem.Config{
				Port:           cfg.Modem.Port,
				BaudRate:       cfg.Modem.BaudRate,
				InitTimeout:    cfg.Modem.InitTimeout,
				CommandTimeout: cfg.Modem.CommandTimeout,
			}
		}),
	)
}
