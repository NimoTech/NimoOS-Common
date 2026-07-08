package middleware

import (
	"net/http"

	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

func Cors() echo.MiddlewareFunc {
	return echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	})
}

// VersionResponse is the body returned by each service's /version endpoint,
// consumed by the Gateway's component-status aggregation.
type VersionResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// RegisterVersionRoute mounts GET <path> on e, returning {name, version}.
// It only registers the route; services using a global e.Use(JWT) must also
// exempt <path> in their JWT Skipper, and services with group-scoped JWT
// should pass an e that is outside the protected group.
func RegisterVersionRoute(e *echo.Echo, path, name, version string) {
	e.GET(path, func(c echo.Context) error {
		return c.JSON(http.StatusOK, VersionResponse{Name: name, Version: version})
	})
}
