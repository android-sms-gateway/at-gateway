package api

import (
	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

type Handler struct {
	authSvc *auth.Service

	logger *zap.Logger
}

func NewHandler(authSvc *auth.Service, logger *zap.Logger) handler.Handler {
	return &Handler{
		authSvc: authSvc,

		logger: logger,
	}
}

func (h *Handler) Register(router fiber.Router) {
	router.Get("", func(c *fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status": "ok",
		})
	})
}
