package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// Placeholder for additional middleware if needed
func CORS(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set("Access-Control-Allow-Origin", "*")
		c.Response().Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Response().Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if c.Request().Method == "OPTIONS" {
			return c.NoContent(http.StatusOK)
		}
		return next(c)
	}
}