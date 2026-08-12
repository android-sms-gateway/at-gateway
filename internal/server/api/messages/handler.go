package messages

import (
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type Handler struct {
	handler.Base

	logger *zap.Logger
}

func NewHandler(logger *zap.Logger, validator *validator.Validate) handler.Handler {
	return &Handler{
		Base: handler.Base{
			Validator: validator,
		},

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router = router.Group("messages")

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
//	@Param			from			query		string							false	"Start date in RFC3339 format"	Format(date-time)
//	@Param			to				query		string							false	"End date in RFC3339 format"	Format(date-time)
//	@Param			state			query		smsgateway.ProcessingState		false	"Filter messages by processing state"
//	@Param			deviceId		query		string							false	"Filter by device ID"																	minLength(21)	maxLength(21)
//	@Param			limit			query		int								false	"Pagination limit"																		default(50)		minimum(1)	maximum(100)//	@Param	offset	query	int	false	"Pagination offset"	default(0)
//	@Param			offset			query		int								false	"Pagination offset"																		default(0)
//	@Param			includeContent	query		bool							false	"Include textMessage/dataMessage content for each message. Default is false"			default(false)
//	@Param			sort			query		string							false	"Sort order per JSON:API spec. Use created_at (ascending) or -created_at (descending)"	Enums(created_at, -created_at)	default(-created_at)
//	@Success		200				{object}	smsgateway.GetMessagesResponse	"A list of messages"
//	@Header			200				{integer}	X-Total-Count					"Total number of items available"
//	@Failure		400				{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401				{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		403				{object}	smsgateway.ErrorResponse		"Forbidden"
//	@Failure		500				{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Router			/messages [get]
func (h *Handler) list(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }

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
//	@Failure		500	{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Router			/messages/{id} [get]
func (h *Handler) get(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }

// Enqueue message.
//
//	@Summary		Enqueue message
//	@Description	Enqueues a message for sending. If `deviceId` is set, the specified device is used; otherwise a random registered device is chosen.
//	@Tags			User, Messages
//	@Accept			json
//	@Produce		json
//	@Param			skipPhoneValidation	query		bool							false	"Skip phone validation"
//	@Param			deviceActiveWithin	query		int								false	"Filter devices active within the specified number of hours"	default(0)	minimum(0)
//	@Param			request				body		smsgateway.Message				true	"Send message request"
//	@Success		202					{object}	smsgateway.GetMessageResponse	"Message enqueued"
//	@Failure		400					{object}	smsgateway.ErrorResponse		"Invalid request"
//	@Failure		401					{object}	smsgateway.ErrorResponse		"Unauthorized"
//	@Failure		403					{object}	smsgateway.ErrorResponse		"Forbidden"
//	@Failure		409					{object}	smsgateway.ErrorResponse		"Message with such ID already exists"
//	@Failure		500					{object}	smsgateway.ErrorResponse		"Internal server error"
//	@Failure		503					{object}	smsgateway.ErrorResponse		"Queue limits exceeded; ensure device is online"
//	@Header			202					{string}	Location						"Get message state URL"
//	@Router			/messages [post]
func (h *Handler) post(_ *fiber.Ctx, _ *smsgateway.Message) error { return fiber.ErrNotImplemented }

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
func (h *Handler) delete(_ *fiber.Ctx) error { return fiber.ErrNotImplemented }
