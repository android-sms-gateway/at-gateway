package modem

import (
	"fmt"
	"io"

	"go.bug.st/serial"
)

const serialDataBits = 8

type Port interface {
	io.ReadWriteCloser
}

func OpenPort(portName string, baudRate int) (Port, error) {
	mode := &serial.Mode{
		BaudRate:          baudRate,
		DataBits:          serialDataBits,
		InitialStatusBits: nil,
		Parity:            serial.NoParity,
		StopBits:          serial.OneStopBit,
	}
	p, err := serial.Open(portName, mode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", portName, err)
	}
	return p, nil
}
