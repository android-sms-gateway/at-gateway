package devices

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/gofiber/fiber/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

type Handler struct {
	devicesSvc *devices.Service
	modemSvc   *modem.Service

	logger *zap.Logger
}

func NewHandler(devicesSvc *devices.Service, modemSvc *modem.Service, logger *zap.Logger) handler.Handler {
	return &Handler{
		devicesSvc: devicesSvc,
		modemSvc:   modemSvc,

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("devices")

	router.Get("", h.list)
	router.Delete(":id", h.delete)
}

// List devices.
//
//	@Summary		List devices
//	@Description	Returns list of registered devices
//	@Tags			User, Devices
//	@Produce		json
//	@Success		200	{object}	[]smsgateway.Device			"Device list"
//	@Failure		400	{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401	{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500	{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/devices [get]
func (h *Handler) list(c *fiber.Ctx) error {
	device := h.devicesSvc.Get()
	sim := h.modemSvc.SIM()

	return c.JSON(
		[]smsgateway.Device{
			{
				ID:        device.ID,
				Name:      device.Name,
				CreatedAt: device.CreatedAt,
				UpdatedAt: device.CreatedAt,
				DeletedAt: nil,
				LastSeen:  time.Now(),
				SimCards: []smsgateway.SimCard{{
					SlotIndex:   0,
					SimNumber:   1,
					PhoneNumber: lo.ToPtr(sim.PhoneNumber),
					CarrierName: lo.ToPtr(sim.Carrier),
					ICCID:       lo.ToPtr(sim.ICCID),
				}},
			},
		},
	)
}

// Remove device.
//
//	@Summary		Remove device
//	@Description	Removes device
//	@Tags			User, Devices
//	@Produce		json
//	@Param			id	path	string	true	"Device ID"
//	@Success		204	"Successfully removed"
//	@Failure		400	{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401	{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		404	{object}	smsgateway.ErrorResponse	"Device not found"
//	@Failure		500	{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Failure		501	{object}	smsgateway.ErrorResponse	"Not implemented"
//	@Router			/devices/{id} [delete]
func (h *Handler) delete(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }
