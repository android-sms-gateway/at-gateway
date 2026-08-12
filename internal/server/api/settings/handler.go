package settings

import (
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

type Handler struct {
	handler.Base
}

func NewHandler(validator *validator.Validate) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("settings")

	router.Get("", h.list)
	router.Put("", validation.DecorateWithBodyEx(h.Validator, h.replace))
	router.Patch("", validation.DecorateWithBodyEx(h.Validator, h.update))
}

// Get settings.
//
//	@Summary		Get settings
//	@Description	Returns settings for a specific user
//	@Tags			User, Settings
//	@Produce		json
//	@Success		200	{object}	smsgateway.DeviceSettings	"Settings"
//	@Failure		401	{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500	{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/settings [get]
func (h *Handler) list(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }

// Replace settings.
//
//	@Summary		Replace settings
//	@Description	Replaces settings
//	@Tags			User, Settings
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.DeviceSettings	true	"Settings"
//	@Success		200		{object}	object						"Settings updated"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/settings [put]
func (h *Handler) replace(_ *fiber.Ctx, _ *smsgateway.DeviceSettings) error {
	return fiber.ErrNotImplemented
}

// Partially update settings.
//
//	@Summary		Partially update settings
//	@Description	Partially updates settings for a specific user
//	@Tags			User, Settings
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.DeviceSettings	true	"Settings"
//	@Success		200		{object}	object						"Settings updated"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/settings [patch]
func (h *Handler) update(_ *fiber.Ctx, _ *smsgateway.DeviceSettings) error {
	return fiber.ErrNotImplemented
}
