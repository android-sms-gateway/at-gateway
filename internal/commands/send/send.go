package send

import (
	"context"
	"fmt"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/config"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/urfave/cli/v3"
	"github.com/warthog618/modem/at"
	"go.uber.org/zap"
)

// fallbackCommandTimeout mirrors the modem service fallback (5s) for
// CommandTimeout <= 0 configs: the library's at.WithTimeout(0) means IMMEDIATE
// timeout, never pass the raw value through.
const fallbackCommandTimeout = 5 * time.Second

// Command returns the one-shot SMS send command: a visible vertical slice of
// the modem send path without the HTTP server or the database.
func Command() *cli.Command {
	return &cli.Command{
		Name:  "send",
		Usage: "Send one SMS via the modem",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "phone",
				Aliases:  []string{"p"},
				Usage:    "Destination phone number",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "text",
				Aliases:  []string{"t"},
				Usage:    "SMS text: printable 7-bit ASCII, newline, carriage return, max 160 characters",
				Required: true,
			},
			&cli.IntFlag{
				Name:    "sim",
				Aliases: []string{"s"},
				Usage:   "SIM slot (reserved: the single-SIM MVP ignores it)",
				Value:   1,
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Per-command modem timeout (default: modem.command_timeout config)",
			},
		},
		Action: run,
	}
}

// run loads the config, validates the text, opens the modem port and sends
// one SMS. Every failure prints a message and exits 1 via cli.Exit; a
// successful send prints "sent ref=<mr>" and exits 0. Validation happens
// BEFORE any config or modem wiring so an invalid text never touches the
// modem.
func run(ctx context.Context, cmd *cli.Command) error {
	phone := cmd.String("phone")
	text := cmd.String("text")
	sim := cmd.Int("sim")

	if err := modem.ValidateASCII(text); err != nil {
		return cli.Exit(err, 1)
	}

	cfg, err := config.New()
	if err != nil {
		return cli.Exit(fmt.Errorf("load config: %w", err), 1)
	}

	logger, err := zap.NewProduction()
	if err != nil {
		return cli.Exit(fmt.Errorf("create logger: %w", err), 1)
	}
	defer func() { _ = logger.Sync() }()

	p, err := port.Open(port.Config{
		Name:     cfg.Modem.Port,
		BaudRate: cfg.Modem.BaudRate,
	})
	if err != nil {
		return cli.Exit(fmt.Errorf("open modem port %s: %w", cfg.Modem.Port, err), 1)
	}
	defer func() { _ = p.Close() }()

	cmdTimeout := cmd.Duration("timeout")
	if cmdTimeout <= 0 {
		cmdTimeout = cfg.Modem.CommandTimeout
	}
	if cmdTimeout <= 0 {
		cmdTimeout = fallbackCommandTimeout
	}

	a := at.New(p, at.WithTimeout(cmdTimeout))
	commands := modem.NewCommands(a, modem.NewMetrics())

	initCtx, cancelInit := context.WithTimeout(ctx, cfg.Modem.InitTimeout)
	defer cancelInit()
	if initErr := commands.Init(initCtx); initErr != nil {
		return cli.Exit(fmt.Errorf("modem init: %w", initErr), 1)
	}

	logger.Info("sending SMS",
		zap.String("port", cfg.Modem.Port),
		zap.Int("baud", cfg.Modem.BaudRate),
		zap.Int("sim", sim),
	)

	ref, err := commands.SendSMS(ctx, phone, text)
	if err != nil {
		return cli.Exit(fmt.Errorf("send SMS: %w", err), 1)
	}

	_, _ = fmt.Fprintf(cmd.Root().Writer, "sent ref=%d\n", ref)

	return nil
}
