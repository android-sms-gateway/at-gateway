package webhooks

import (
	"errors"
	"fmt"

	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type Handler struct {
	handler.Base

	webhooksSvc *webhooks.Service

	logger *zap.Logger
}

// NewHandler wires the webhooks registry endpoints to the service layer,
// which owns event/device validation and is the sole id generator.
func NewHandler(
	webhooksSvc *webhooks.Service,
	logger *zap.Logger,
	validator *validator.Validate,
) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},

		webhooksSvc: webhooksSvc,

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("webhooks", h.errorHandler)

	router.Get("", h.list)
	router.Post("", validation.DecorateWithBodyEx(h.Validator, h.post))
	router.Delete(":id", h.delete)
}

// List webhooks.
//
//	@Summary		List webhooks
//	@Description	Returns list of registered webhooks
//	@Tags			User, Webhooks
//	@Produce		json
//	@Success		200	{object}	[]smsgateway.Webhook		"Webhook list"
//	@Failure		401	{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500	{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/webhooks [get]
func (h *Handler) list(c *fiber.Ctx) error {
	items, err := h.webhooksSvc.Select(c.Context())
	if err != nil {
		return fmt.Errorf("select webhooks: %w", err)
	}

	return c.JSON(items)
}

// Register webhook.
//
//	@Summary		Register webhook
//	@Description	Registers webhook. If webhook with same ID already exists, it will be replaced
//	@Tags			User, Webhooks
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.Webhook			true	"Webhook"
//	@Success		201		{object}	smsgateway.Webhook			"Created"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/webhooks [post]
func (h *Handler) post(c *fiber.Ctx, req *smsgateway.Webhook) error {
	webhook := &smsgateway.Webhook{
		ID:       req.ID,
		DeviceID: req.DeviceID,
		URL:      req.URL,
		Event:    req.Event,
	}
	if err := h.webhooksSvc.Replace(c.Context(), webhook); err != nil {
		return fmt.Errorf("replace webhook: %w", err)
	}

	// Echo the dto exactly as Replace left it: id generated, deviceId =
	// client-supplied local id or null (never the filled local device id).
	return c.Status(fiber.StatusCreated).JSON(webhook)
}

// Delete webhook.
//
//	@Summary		Delete webhook
//	@Description	Deletes webhook
//	@Tags			User, Webhooks
//	@Produce		json
//	@Param			id	path	string	true	"Webhook ID"
//	@Success		204	"Successfully removed"
//	@Failure		401	{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500	{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/webhooks/{id} [delete]
func (h *Handler) delete(c *fiber.Ctx) error {
	if err := h.webhooksSvc.Delete(c.Context(), c.Params("id")); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// errorHandler maps registry service sentinels onto wire 4xx statuses; the
// global JSON error handler then renders {message, code} with the full wrap
// chain byte-exact (server parity).
func (h *Handler) errorHandler(c *fiber.Ctx) error {
	err := c.Next()
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, webhooks.ErrInvalidEvent):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, webhooks.ErrDeviceNotFound):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	// Preserve error types for the global handler, which maps unknown errors to 500.
	return fmt.Errorf("webhook handler: %w", err)
}
