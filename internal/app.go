package internal

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/android-sms-gateway/at-gateway/internal/commands"
	"github.com/go-core-fx/healthfx"
	"github.com/samber/lo"
	"github.com/urfave/cli/v3"
)

func Run(version healthfx.Version) {
	app := &cli.Command{
		Name:           "at-gateway",
		Usage:          "SMS Gateway daemon using AT-command modems",
		Description:    "SMS Gateway daemon using AT-command modems",
		Version:        version.Version,
		DefaultCommand: "serve",
		Flags:          []cli.Flag{},
		Commands:       commands.Commands(version),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	if err := app.Run(ctx, os.Args); err != nil {
		exitCode := 1
		if exitErr, ok := lo.ErrorsAs[cli.ExitCoder](err); ok {
			exitCode = exitErr.ExitCode()
		}

		stop()
		os.Exit(exitCode)
	}

	stop()
}
