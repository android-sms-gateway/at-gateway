package userauth

import (
	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
)

func NewBasic(authSvc *auth.Service) fiber.Handler {
	return basicauth.New(basicauth.Config{
		Next:  nil,
		Users: nil,
		Realm: "",
		Authorizer: func(username, password string) bool {
			return authSvc.ValidateBasic(username, password) == nil
		},
		Unauthorized:    nil,
		ContextUsername: nil,
		ContextPassword: nil,
	})
}
