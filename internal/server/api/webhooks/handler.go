package webhooks

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
	router = router.Group("webhooks")

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
func (h *Handler) list(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }

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
func (h *Handler) post(_ *fiber.Ctx, _ *smsgateway.Webhook) error { return fiber.ErrNotImplemented }

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
func (h *Handler) delete(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }
