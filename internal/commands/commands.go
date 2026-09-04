package commands

import (
	"github.com/android-sms-gateway/at-gateway/internal/commands/send"
	"github.com/android-sms-gateway/at-gateway/internal/commands/serve"
	"github.com/go-core-fx/healthfx"
	"github.com/urfave/cli/v3"
)

// Commands returns the root CLI commands (serve and send) wired with the
// build version.
func Commands(version healthfx.Version) []*cli.Command {
	return []*cli.Command{
		serve.Command(version),
		send.Command(),
	}
}
