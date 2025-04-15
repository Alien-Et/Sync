package api

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/random"
	"WebdavSync/internal/db"
	"WebdavSync/internal/engine"
	"WebdavSync/internal/models"
)

type API struct {
	eng           *engine.SyncEngine
	db            *db.DB
	conflictQueue map[string]chan string
	mu            sync.Mutex
}

func SetupRoutes(g *echo.Group, eng *engine.SyncEngine, db *db.DB) {
	api := &API{
		eng:           eng,
		db:            db,
		conflictQueue: make(map[string]chan string),
	}
	go api.handleConflicts()

	// Public routes
	g.POST("/login", api.login)

	// Protected routes
	protected := g.Group("", api.authMiddleware)
	protected.GET("/config", api.getConfig)
	protected.PUT("/config", api.updateConfig)
	protected.GET("/status", api.getStatus)
	protected.POST("/pause", api.pause)
	protected.POST("/resume", api.resume)
	protected.GET("/files", api.getFiles)
	protected.GET("/tasks", api.getTasks)
	protected.GET("/logs", api.getLogs)
	protected.POST("/resolve-conflict", api.resolveConflict)
}

func (a *API) login(c echo.Context) error {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.Bind(&creds); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}
	if a.db.AuthenticateUser(creds.Username, creds.Password) {
		return c.JSON(http.StatusOK, map[string]string{"token": "dummy-token"}) // Replace with proper JWT in production
	}
	return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Invalid credentials"})
}

func (a *API) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// In production, validate JWT token here
		token := c.Request().Header.Get("Authorization")
		if token == "" {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "Missing token"})
		}
		return next(c)
	}
}

func (a *API) getConfig(c echo.Context) error {
	cfg, err := models.Load(a.db.DB)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, cfg)
}

func (a *API) updateConfig(c echo.Context) error {
	var cfg models.Config
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}
	if err := models.Save(a.db.DB, cfg); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	a.eng.UpdateConfig(cfg)
	return c.JSON(http.StatusOK, map[string]string{"message": "Config updated"})
}

func (a *API) getStatus(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]interface{}{
		"paused":           a.eng.IsPaused(),
		"networkAvailable": a.eng.networkAvailable,
	})
}

func (a *API) pause(c echo.Context) error {
	a.eng.Pause()
	return c.JSON(http.StatusOK, map[string]string{"message": "Paused"})
}

func (a *API) resume(c echo.Context) error {
	a.eng.Resume()
	return c.JSON(http.StatusOK, map[string]string{"message": "Resumed"})
}

func (a *API) getFiles(c echo.Context) error {
	files, err := a.db.GetFiles()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, files)
}

func (a *API) getTasks(c echo.Context) error {
	tasks, err := a.db.GetPendingTasks()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, tasks)
}

func (a *API) getLogs(c echo.Context) error {
	id := random.String(16)
	logChan := a.eng.SubscribeLogs(id)
	defer a.eng.UnsubscribeLogs(id)

	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("Connection", "keep-alive")
	c.Response().WriteHeader(http.StatusOK)

	for {
		select {
		case log := <-logChan:
			fmt.Fprintf(c.Response(), "data: %s\n\n", log.Message)
			c.Response().Flush()
		case <-c.Request().Context().Done():
			return nil
		}
	}
}

func (a *API) handleConflicts() {
	for conflict := range a.eng.Conflicts() {
		id := random.String(16)
		a.mu.Lock()
		a.conflictQueue[id] = conflict.Choice
		a.mu.Unlock()

		// Notify clients via logs (in lieu of WebSocket for conflicts)
		a.eng.log(fmt.Sprintf("New conflict detected for file: %s (ID: %s)", conflict.File.Path, id))
	}
}

func (a *API) resolveConflict(c echo.Context) error {
	var req struct {
		ID         string `json:"id"`
		Resolution string `json:"resolution"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}
	a.mu.Lock()
	choice, exists := a.conflictQueue[req.ID]
	if !exists {
		a.mu.Unlock()
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Conflict not found"})
	}
	delete(a.conflictQueue, req.ID)
	a.mu.Unlock()
	choice <- req.Resolution
	return c.JSON(http.StatusOK, map[string]string{"message": "Conflict resolved"})
}