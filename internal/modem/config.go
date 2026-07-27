package modem

import "time"

type Config struct {
	Port           string
	BaudRate       int
	InitTimeout    time.Duration
	CommandTimeout time.Duration
}
