package server

import (
	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/server/api"
	"github.com/android-sms-gateway/at-gateway/internal/server/api/devices"
	"github.com/android-sms-gateway/at-gateway/internal/server/api/messages"
	"github.com/android-sms-gateway/at-gateway/internal/server/docs"
	"github.com/android-sms-gateway/at-gateway/internal/server/middlewares/userauth"
	"github.com/go-core-fx/fiberfx"
	"github.com/go-core-fx/fiberfx/handler"
	"github.com/go-core-fx/fiberfx/health"
	"github.com/go-core-fx/fiberfx/openapi"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-core-fx/logger"
	"github.com/gofiber/fiber/v2"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

func Module() fx.Option {
	return fx.Module(
		"server",
		logger.WithNamedLogger("server"),

		fx.Provide(func(log *zap.Logger) fiberfx.Options {
			opts := fiberfx.Options{}
			opts.WithErrorHandler(fiberfx.NewJSONErrorHandler(log))
			opts.WithMetrics()
			return opts
		}),
		fx.Supply(docs.SwaggerInfo),

		fx.Provide(
			health.NewHandler,
			openapi.NewHandler,
			fx.Private,
		),

		fx.Provide(
			fx.Annotate(api.NewHandler, fx.ResultTags(`group:"handlers"`)),
			fx.Annotate(devices.NewHandler, fx.ResultTags(`group:"handlers"`)),
			fx.Annotate(messages.NewHandler, fx.ResultTags(`group:"handlers"`)),
			fx.Private,
		),

		fx.Invoke(
			fx.Annotate(
				func(handlers []handler.Handler, healthHandler *health.Handler, openapiHandler *openapi.Handler, authSvc *auth.Service, app *fiber.App) {
					// Health endpoint
					healthHandler.Register(app)

					// Version 1 API group
					v1 := app.Group("/api/v1", userauth.NewBasic(authSvc))
					openapiHandler.Register(v1.Group("/docs"))

					v1.Use(validation.Middleware)

					for _, h := range handlers {
						h.Register(v1)
					}
				},
				fx.ParamTags(`group:"handlers"`),
			),
		),
	)
}
