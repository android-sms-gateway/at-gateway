package config

import (
	"fmt"
	"os"
	"time"

	"github.com/go-core-fx/config"
)

type http struct {
	Address     string        `koanf:"address"`
	ProxyHeader string        `koanf:"proxy_header"`
	Proxies     []string      `koanf:"proxies"`
	OpenAPI     openAPIConfig `koanf:"openapi"`
}

type openAPIConfig struct {
	Enabled    bool   `koanf:"enabled"`
	PublicHost string `koanf:"public_host"`
	PublicPath string `koanf:"public_path"`
}

type modemConfig struct {
	Port           string        `koanf:"port"`
	BaudRate       int           `koanf:"baud_rate"`
	InitTimeout    time.Duration `koanf:"init_timeout"`
	CommandTimeout time.Duration `koanf:"command_timeout"`
}

type storageConfig struct {
	Path string `koanf:"path"`
}

type authConfig struct {
	Basic authBasicConfig `koanf:"basic"`
}

type authBasicConfig struct {
	Username string `koanf:"username"`
	Password string `koanf:"password"`
}

type deviceConfig struct {
	Name string `koanf:"name"`
}

type databaseConfig struct {
	URL string `koanf:"url"`
}

type messagesConfig struct {
	PollInterval time.Duration `koanf:"poll_interval"`
}

type Config struct {
	HTTP     http           `koanf:"http"`
	Modem    modemConfig    `koanf:"modem"`
	Storage  storageConfig  `koanf:"storage"`
	Auth     authConfig     `koanf:"auth"`
	Device   deviceConfig   `koanf:"device"`
	Database databaseConfig `koanf:"database"`
	Messages messagesConfig `koanf:"messages"`
}

func Default() Config {
	//nolint:mnd // default values
	return Config{
		HTTP: http{
			Address:     "127.0.0.1:3000",
			ProxyHeader: "X-Forwarded-For",
			Proxies:     []string{},
			OpenAPI: openAPIConfig{
				Enabled:    true,
				PublicHost: "",
				PublicPath: "",
			},
		},
		Modem: modemConfig{
			Port:           "/dev/ttyUSB0",
			BaudRate:       115200,
			InitTimeout:    30 * time.Second,
			CommandTimeout: 30 * time.Second,
		},
		Storage: storageConfig{
			Path: "data/storage.json",
		},
		Auth: authConfig{
			Basic: authBasicConfig{
				Username: "sms",
				Password: "",
			},
		},
		Device: deviceConfig{
			Name: "",
		},
		Database: databaseConfig{
			URL: "sqlite://data/gateway.db",
		},
		Messages: messagesConfig{
			PollInterval: time.Second,
		},
	}
}

func New() (Config, error) {
	cfg := Default()

	options := []config.Option{}
	if yamlPath := os.Getenv("CONFIG_PATH"); yamlPath != "" {
		options = append(options, config.WithLocalYAML(yamlPath))
	}

	if err := config.Load(&cfg, options...); err != nil {
		return Config{}, fmt.Errorf("failed to load config: %w", err)
	}

	return cfg, nil
}
