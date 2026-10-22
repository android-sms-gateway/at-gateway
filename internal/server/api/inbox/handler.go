package inbox

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

type Handler struct {
	handler.Base

	inboxSvc *inbox.Service

	logger *zap.Logger
}

// NewHandler wires the inbox endpoints to the service layer, which owns the
// stored messages and the business validation.
func NewHandler(
	inboxSvc *inbox.Service,
	logger *zap.Logger,
	validator *validator.Validate,
) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},

		inboxSvc: inboxSvc,

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("inbox", h.errorHandler)

	router.Get("", h.list)
	router.Post("refresh", validation.DecorateWithBodyEx(h.Validator, h.refresh))
}

// Get incoming messages.
//
//	@Summary		Get incoming messages
//	@Description	Retrieves incoming messages with filtering and pagination.
//	@Tags			User, Inbox
//	@Produce		json
//	@Param			type		query		string						false	"Filter incoming messages by type"			Enums(SMS,DATA_SMS,MMS,MMS_DOWNLOADED)
//	@Param			limit		query		int							false	"Maximum number of messages to return"		minimum(1)	maximum(500)	default(50)
//	@Param			offset		query		int							false	"Number of messages to skip"				minimum(0)	default(0)
//	@Param			from		query		string						false	"Start of date range (ISO 8601, inclusive)"	Format(date-time)
//	@Param			to			query		string						false	"End of date range (ISO 8601, exclusive)"	Format(date-time)
//	@Param			deviceId	query		string						false	"Device ID"
//	@Success		200			{array}		smsgateway.IncomingMessage	"A list of incoming messages"
//	@Header			200			{integer}	X-Total-Count				"Total number of items available"
//	@Failure		400			{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401			{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403			{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500			{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/inbox [get]
func (h *Handler) list(c *fiber.Ctx) error {
	var params listInboxParams
	if err := h.QueryParserValidator(c, &params); err != nil {
		return fmt.Errorf("parse query parameters: %w", validation.NewErrors(err))
	}

	result, total, err := h.inboxSvc.List(c.Context(), params.toListOptions())
	if err != nil {
		return fmt.Errorf("list inbox messages: %w", err)
	}

	c.Set("X-Total-Count", strconv.Itoa(total))

	// The list is serialized through the client-go wire shape; the domain
	// IncomingMessage struct is not JSON-tagged and must never reach the wire
	// raw.
	return c.JSON(
		lo.Map(result, func(message inbox.IncomingMessage, _ int) smsgateway.IncomingMessage {
			return incomingMessageToDTO(message)
		}),
	)
}

// Request inbox refresh.
//
//	@Summary		Request inbox messages refresh
//	@Description	Refreshes inbox messages. Webhooks are triggered when triggerWebhooks is true.
//	@Tags			User, Inbox
//	@Accept			json
//	@Produce		json
//	@Param			request	body	smsgateway.InboxRefreshRequest	true	"Refresh inbox request"
//	@Success		202		"Inbox refresh request accepted"
//	@Failure		400		{object}	smsgateway.ErrorResponse	"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse	"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse	"Forbidden"
//	@Failure		500		{object}	smsgateway.ErrorResponse	"Internal server error"
//	@Router			/inbox/refresh [post]
func (h *Handler) refresh(_ *fiber.Ctx, _ *smsgateway.InboxRefreshRequest) error {
	return fiber.ErrNotImplemented
}

// errorHandler maps the inbox sentinels onto their HTTP status codes.
func (h *Handler) errorHandler(c *fiber.Ctx) error {
	err := c.Next()
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, inbox.ErrDeviceNotFound):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, inbox.ErrInvalidDateRange):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	return err
}
