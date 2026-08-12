package modem

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/at"
)

type CommandsConfig struct {
	CommandTimeout time.Duration
}

type Commands struct {
	at     *at.AT
	config CommandsConfig
}

// NewCommands creates a new Commands instance.
func NewCommands(at *at.AT, config CommandsConfig) *Commands {
	return &Commands{
		at:     at,
		config: config,
	}
}

func (c *Commands) Init(ctx context.Context) error {
	commands := []struct {
		cmd string
		tag string
	}{
		{"AT", "test"},
		{"ATE0", "echo off"},
		{"AT+CMEE=1", "verbose errors"},
		{"AT+CMGF=1", "text mode"},
		{"AT+CNMI=2,1,0,0,0", "SMS routing"},
		{"AT+CPIN?", "SIM PIN"},
	}

	for _, cmd := range commands {
		resp, err := c.at.Exec(ctx, cmd.cmd)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.cmd, err)
		}
		if cmd.tag == "SIM PIN" {
			for _, line := range resp.Lines {
				if suffix, ok := strings.CutPrefix(line, "+CPIN:"); ok {
					status := strings.TrimSpace(suffix)
					if status != "READY" {
						return fmt.Errorf("%s: %s: %w", cmd.tag, status, ErrSIMNotReady)
					}
				}
			}
		}
	}

	return nil
}

func (c *Commands) GetModemInfo(ctx context.Context) (Info, error) {
	info := Info{
		Manufacturer: "",
		Model:        "",
		IMEI:         "",
	}

	cmdCtx, cancel := context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()

	manufacturer, err := c.atGetString(cmdCtx, "AT+GMI")
	if err != nil {
		return info, fmt.Errorf("manufacturer: %w", err)
	}
	info.Manufacturer = manufacturer

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()

	model, err := c.atGetString(cmdCtx, "AT+GMM")
	if err != nil {
		return info, fmt.Errorf("model: %w", err)
	}
	info.Model = model

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()

	imei, err := c.atGetString(cmdCtx, "AT+GSN")
	if err != nil {
		return info, fmt.Errorf("IMEI: %w", err)
	}
	info.IMEI = imei

	return info, nil
}

func (c *Commands) GetSimInfo(ctx context.Context) (SimInfo, error) {
	info := SimInfo{
		PhoneNumber:       "",
		ICCID:             "",
		Carrier:           "",
		NetworkRegistered: false,
		SignalQuality:     0,
		SignalPercent:     0,
	}

	cmdCtx, cancel := context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()
	info.PhoneNumber = c.atGetCNUM(cmdCtx)

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()
	info.ICCID = c.atGetFirstLine(cmdCtx, "AT+CCID")

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()
	info.Carrier = c.atGetCOPS(cmdCtx)

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()
	info.SignalQuality, info.SignalPercent = c.atGetCSQ(cmdCtx)

	cmdCtx, cancel = context.WithTimeout(ctx, c.config.CommandTimeout)
	defer cancel()
	info.NetworkRegistered = c.atGetCREG(cmdCtx)

	return info, nil
}

func (c *Commands) atGetString(ctx context.Context, cmd string) (string, error) {
	resp, err := c.at.Exec(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}
	if len(resp.Lines) > 0 {
		return strings.TrimSpace(resp.Lines[0]), nil
	}
	return "", nil
}

func (c *Commands) atGetFirstLine(ctx context.Context, cmd string) string {
	resp, err := c.at.Exec(ctx, cmd)
	if err != nil || len(resp.Lines) == 0 {
		return ""
	}
	return strings.TrimSpace(resp.Lines[0])
}

func (c *Commands) atGetCNUM(ctx context.Context) string {
	resp, err := c.at.Exec(ctx, "AT+CNUM")
	if err != nil {
		return ""
	}
	for _, line := range resp.Lines {
		if _, after, found := strings.Cut(line, "+CNUM:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 2 { //nolint:mnd // CNUM format: name,number,type
				return strings.Trim(parts[1], "\"")
			}
		}
	}
	return ""
}

func (c *Commands) atGetCOPS(ctx context.Context) string {
	resp, err := c.at.Exec(ctx, "AT+COPS?")
	if err != nil {
		return ""
	}
	for _, line := range resp.Lines {
		if _, after, found := strings.Cut(line, "+COPS:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 3 { //nolint:mnd // COPS format: mode,format,name,act
				return strings.Trim(parts[2], "\"")
			}
		}
	}
	return ""
}

func (c *Commands) atGetCSQ(ctx context.Context) (int, int) {
	resp, err := c.at.Exec(ctx, "AT+CSQ")
	if err != nil || len(resp.Lines) == 0 {
		return 0, 0
	}
	for _, line := range resp.Lines {
		if _, after, found := strings.Cut(line, "+CSQ:"); found {
			return c.parseCSQ(after)
		}
	}
	return 0, 0
}

func (c *Commands) atGetCREG(ctx context.Context) bool {
	resp, err := c.at.Exec(ctx, "AT+CREG?")
	if err != nil {
		return false
	}
	for _, line := range resp.Lines {
		if _, after, found := strings.Cut(line, "+CREG:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 2 { //nolint:mnd // CREG format: stat,...
				status := strings.TrimSpace(parts[1])
				return status == "1" || status == "5"
			}
		}
	}
	return false
}

func (c *Commands) parseCSQ(raw string) (int, int) {
	val := strings.TrimSpace(strings.SplitN(raw, ",", 2)[0]) //nolint:mnd // always 2 parts
	rssi, err := strconv.Atoi(val)
	if err != nil {
		return 0, 0
	}
	if rssi < 0 || rssi > 31 {
		return rssi, 0
	}
	return rssi, rssi * 100 / 31 //nolint:mnd // 100% scale
}
