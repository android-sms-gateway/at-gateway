package devices

import "time"

type Device struct {
	ID        string
	Name      string
	CreatedAt time.Time
}
