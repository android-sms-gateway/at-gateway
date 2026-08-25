package config

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/go-core-fx/fiberfx"
	"github.com/go-core-fx/fiberfx/openapi"
	"github.com/go-core-fx/sqlfx"
	"go.uber.org/fx"
)

func Module() fx.Option {
	//nolint:mnd //default values
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
		fx.Provide(
			func(cfg Config) modem.Config {
				return modem.Config{
					Port:           cfg.Modem.Port,
					BaudRate:       cfg.Modem.BaudRate,
					InitTimeout:    cfg.Modem.InitTimeout,
					CommandTimeout: cfg.Modem.CommandTimeout,
				}
			},
			func(cfg Config) storage.Config {
				return storage.Config{
					Path: cfg.Storage.Path,
				}
			},
			func(cfg Config) auth.Config {
				return auth.Config{
					Basic: auth.BasicConfig{
						Username: cfg.Auth.Basic.Username,
						Password: cfg.Auth.Basic.Password,
					},
				}
			},
			func(cfg Config) devices.Config {
				return devices.Config{
					Name: cfg.Device.Name,
				}
			},
			func(cfg Config) messages.Config {
				return messages.Config{
					PollInterval: cfg.Messages.PollInterval,
					DeviceID:     cfg.Messages.DeviceID,
				}
			},
			func(cfg Config) sqlfx.Config {
				return sqlfx.Config{
					URL:             cfg.Database.URL,
					ConnMaxIdleTime: 20 * time.Minute,
					ConnMaxLifetime: time.Hour,
					MaxOpenConns:    1,
					MaxIdleConns:    1,
				}
			},
		),
	)
}
