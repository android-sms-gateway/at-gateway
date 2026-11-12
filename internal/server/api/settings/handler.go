package settings

import (
	"errors"
	"fmt"

	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
)

// Handler serves the settings API by delegating to the settings aggregator:
// wire binding and validation only, no knowledge of module sections or
// fields. The client-go struct tags are the edge validator via
// validation.DecorateWithBodyEx - including the isdefault tag on
// signing_key, whose rejection of every non-empty value is pending a
// client-go fix.
type Handler struct {
	handler.Base

	settingsSvc *settings.Service
}

// NewHandler wires the settings API with its aggregator and validator.
func NewHandler(
	settingsSvc *settings.Service,
	validator *validator.Validate,
) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},

		settingsSvc: settingsSvc,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("settings", h.errorHandler)

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
func (h *Handler) list(c *fiber.Ctx) error {
	settings, err := h.settingsSvc.Get(c.Context())
	if err != nil {
		return fmt.Errorf("get settings: %w", err)
	}

	return c.JSON(settings)
}

// Replace settings.
//
//	@Summary		Replace settings
//	@Description	Replaces settings
//	@Tags			User, Settings
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.DeviceSettings	true	"Settings"
//	@Success		200		{object}	smsgateway.DeviceSettings	"Settings updated"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/settings [put]
func (h *Handler) replace(c *fiber.Ctx, req *smsgateway.DeviceSettings) error {
	settings, err := h.settingsSvc.Replace(c.Context(), *req)
	if err != nil {
		return fmt.Errorf("replace settings: %w", err)
	}

	return c.JSON(settings)
}

// Partially update settings.
//
//	@Summary		Partially update settings
//	@Description	Partially updates settings for a specific user
//	@Tags			User, Settings
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.DeviceSettings	true	"Settings"
//	@Success		200		{object}	smsgateway.DeviceSettings	"Settings updated"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/settings [patch]
func (h *Handler) update(c *fiber.Ctx, req *smsgateway.DeviceSettings) error {
	settings, err := h.settingsSvc.Update(c.Context(), *req)
	if err != nil {
		return fmt.Errorf("update settings: %w", err)
	}

	return c.JSON(settings)
}

// errorHandler maps the section validation sentinel onto a wire 400. The
// edge tags normally reject first; the sections stay the rule owners (and
// this mapping the wire path) if the client-go tag set changes. Every other
// error keeps travelling to the global handler (500).
func (h *Handler) errorHandler(c *fiber.Ctx) error {
	err := c.Next()
	if err == nil {
		return nil
	}

	if errors.Is(err, settings.ErrInvalidValue) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	// Preserve error types for the global handler, which maps unknown errors to 500.
	return err
}
