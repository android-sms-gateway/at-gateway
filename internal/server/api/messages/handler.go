package messages

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/android-sms-gateway/at-gateway/internal/messages"
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

	messagesSvc *messages.Service

	logger *zap.Logger
}

// NewHandler wires the messages endpoints to the service layer, which owns
// all business validation and the background send worker.
func NewHandler(
	messagesSvc *messages.Service,
	logger *zap.Logger,
	validator *validator.Validate,
) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},

		messagesSvc: messagesSvc,

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("messages", h.errorHandler)

	router.Get("", h.list)
	router.Get(":id", h.get)
	router.Post("", validation.DecorateWithBodyEx(h.Validator, h.post))
	router.Delete(":id", h.delete)
}

// Get message history.
//
//	@Summary		Get messages
//	@Description	Retrieves a list of messages with filtering and pagination
//	@Tags			User, Messages
//	@Produce		json
//	@Param			from			query		string							false	"Start date in RFC3339 format (ignored in MVP)"	Format(date-time)
//	@Param			to				query		string							false	"End date in RFC3339 format (ignored in MVP)"	Format(date-time)
//	@Param			state			query		smsgateway.ProcessingState		false	"Filter messages by processing state"
//	@Param			deviceId		query		string							false	"Filter by device ID (ignored in MVP)"													minLength(21)	maxLength(21)
//	@Param			limit			query		int								false	"Pagination limit"																		default(50)		minimum(1)	maximum(100)
//	@Param			offset			query		int								false	"Pagination offset"																		default(0)
//	@Param			includeContent	query		bool							false	"Include textMessage/dataMessage content for each message (ignored in MVP)"				default(false)
//	@Param			sort			query		string							false	"Sort order per JSON:API spec. Use created_at (ascending) or -created_at (descending)"	Enums(created_at, -created_at)	default(-created_at)
//	@Success		200				{object}	smsgateway.GetMessagesResponse	"A list of messages"
//	@Header			200				{integer}	X-Total-Count					"Total number of items available"
//	@Failure		400				{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401				{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		500				{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Router			/messages [get]
func (h *Handler) list(c *fiber.Ctx) error {
	var options smsgateway.ListMessagesOptions
	if err := h.QueryParserValidator(c, &options); err != nil {
		return fmt.Errorf("parse query parameters: %w", err)
	}

	filter := messages.ListOptions{
		Filter: &messages.ListFilter{
			State:    (*smsgateway.ProcessingState)(options.State),
			DeviceID: options.DeviceID,
			Since:    options.From,
			Until:    options.To,
		},
		Order: &messages.ListOrder{
			Order: (*messages.SortOrder)(options.Sort),
		},
		Pagination: &messages.ListPagination{
			Limit:  options.Limit,
			Offset: options.Offset,
		},
		Flags: &messages.ListFlags{
			IncludeContent: options.IncludeContent,
		},
	}

	result, total, err := h.messagesSvc.List(c.Context(), filter)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}

	c.Set("X-Total-Count", strconv.Itoa(total))

	// The list is serialized through the client-go wire shape; the domain
	// Message struct is not JSON-tagged and must never reach the wire raw.
	return c.JSON(
		lo.Map(result, func(m messages.Message, _ int) smsgateway.MessageState {
			return messageToState(&m)
		}),
	)
}

// Get message state.
//
//	@Summary		Get message state
//	@Description	Returns message state by ID
//	@Tags			User, Messages
//	@Produce		json
//	@Param			id	path		string							true	"Message ID"
//	@Success		200	{object}	smsgateway.GetMessageResponse	"Message state"
//	@Failure		400	{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401	{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse		"Forbidden"
//	@Failure		404	{object}	smsgateway.ErrorResponse		"Message not found"
//	@Failure		500	{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Router			/messages/{id} [get]
func (h *Handler) get(c *fiber.Ctx) error {
	state, err := h.messagesSvc.Get(c.Context(), c.Params("id"))
	if err != nil {
		return fmt.Errorf("get message state: %w", err)
	}

	return c.JSON(smsgateway.GetMessageResponse(messageToState(state)))
}

// Enqueue message.
//
//	@Summary		Enqueue message
//	@Description	Enqueues a message for sending. The single registered device is used; `deviceId` is accepted but ignored.
//	@Tags			User, Messages
//	@Accept			json
//	@Produce		json
//	@Param			request	body		smsgateway.Message				true	"Send message request"
//	@Success		202		{object}	smsgateway.GetMessageResponse	"Message enqueued"
//	@Failure		400		{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401		{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		403		{object}	smsgateway.ErrorResponse		"Forbidden"
//	@Failure		409		{object}	smsgateway.ErrorResponse		"Message with such ID already exists"
//	@Failure		500		{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Header			202		{string}	Location						"Get message state URL"
//	@Router			/messages [post]
func (h *Handler) post(c *fiber.Ctx, req *smsgateway.Message) error {
	input := messageInputFromDTO(req)

	state, err := h.messagesSvc.Enqueue(c.Context(), *input)
	if err != nil {
		return fmt.Errorf("enqueue message: %w", err)
	}

	c.Location("/api/v1/messages/" + state.ID)

	return c.Status(fiber.StatusAccepted).JSON(smsgateway.GetMessageResponse(messageToState(state)))
}

// Cancel message.
//
//	@Summary		Cancel message
//	@Description	Cancels a pending message by ID. The message must be in Pending state.
//	@Tags			User, Messages
//	@Param			id	path		string							true	"Message ID"
//	@Success		200	{object}	smsgateway.GetMessageResponse	"Message state after cancellation"
//	@Failure		400	{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401	{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		403	{object}	smsgateway.ErrorResponse		"Forbidden"
//	@Failure		404	{object}	smsgateway.ErrorResponse		"Message not found"
//	@Failure		500	{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Router			/messages/{id} [delete]
func (h *Handler) delete(c *fiber.Ctx) error {
	state, err := h.messagesSvc.Cancel(c.Context(), c.Params("id"))
	if err != nil {
		return fmt.Errorf("cancel message: %w", err)
	}

	return c.JSON(smsgateway.GetMessageResponse(messageToState(state)))
}

func (h *Handler) errorHandler(c *fiber.Ctx) error {
	err := c.Next()
	if err == nil {
		return nil
	}

	switch {
	case errors.Is(err, messages.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, err.Error())
	case errors.Is(err, messages.ErrAlreadyExists):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, messages.ErrNotPending):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, messages.ErrDuplicateRecipient):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, messages.ErrNotSupported):
		return fiber.NewError(fiber.StatusNotImplemented, err.Error())
	case errors.Is(err, messages.ErrInvalidContent):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, messages.ErrInvalidPhoneNumbers):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}

	return err //nolint:wrapcheck // already wrapped
}
