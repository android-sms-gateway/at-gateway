package port

import (
	"fmt"
	"io"

	"go.bug.st/serial"
)

const serialDataBits = 8

type Port interface {
	io.ReadWriteCloser
}

func Open(config Config) (Port, error) {
	mode := &serial.Mode{
		BaudRate:          config.BaudRate,
		DataBits:          serialDataBits,
		InitialStatusBits: nil,
		Parity:            serial.NoParity,
		StopBits:          serial.OneStopBit,
	}
	p, err := serial.Open(config.Name, mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", config.Name, err)
	}
	return p, nil
}
