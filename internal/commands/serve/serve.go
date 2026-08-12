package serve

import (
	"context"
	"fmt"

	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/config"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/server"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/go-core-fx/fiberfx"
	"github.com/go-core-fx/healthfx"
	"github.com/go-core-fx/logger"
	"github.com/go-core-fx/validatorfx"
	"github.com/urfave/cli/v3"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// Command returns the serve command that starts the full application.
func Command(version healthfx.Version) *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Start the HTTP server and modem service",
		Action: func(ctx context.Context, _ *cli.Command) error {
			return run(ctx, version)
		},
	}
}

func run(ctx context.Context, version healthfx.Version) error {
	app := fx.New(
		// CORE MODULES
		logger.Module(),
		logger.WithFxDefaultLogger(),
		// badgerfx.Module(),
		// bunfx.Module(),
		// cachefx.Module(),
		fiberfx.Module(),
		// gocqlfx.Module(),
		// gocqlxfx.Module(),
		// sqlfx.Module(),
		// goosefx.Module(),
		// gormfx.Module(),
		healthfx.Module(),
		// openrouterfx.Module(),
		// redisfx.Module(),
		// sqlxfx.Module(),
		// telegofx.Module(true),
		validatorfx.Module(),
		// watermillfx.Module(),
		//
		// APP MODULES
		config.Module(),
		server.Module(),
		storage.Module(),
		//
		// BUSINESS MODULES
		modem.Module(true),
		auth.Module(),
		devices.Module(),
		fx.Supply(version),

		fx.Invoke(func(lc fx.Lifecycle, logger *zap.Logger) {
			lc.Append(fx.Hook{
				OnStart: func(_ context.Context) error {
					logger.Info("app started")
					return nil
				},
				OnStop: func(_ context.Context) error {
					logger.Info("app stopped")
					return nil
				},
			})
		}),
	)

	startCtx, cancelStart := context.WithTimeout(ctx, app.StartTimeout())
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	select {
	case <-ctx.Done():
	case <-app.Done():
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), app.StopTimeout())
	defer cancelStop()

	if err := app.Stop(stopCtx); err != nil {
		return fmt.Errorf("failed to stop app: %w", err)
	}

	return nil
}
