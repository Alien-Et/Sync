package main

import (
	"WebdavSync/internal/api"
	"WebdavSync/internal/db"
	"WebdavSync/internal/engine"
	"WebdavSync/internal/models"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

func main() {
	// Initialize database
	db, err := db.NewDB("sync.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// Load configuration
	cfg, err := models.Load(db.DB)
	if err != nil {
		cfg = models.DefaultConfig()
	}

	// Initialize sync engine
	eng := engine.NewSyncEngine(cfg, db.DB)

	// Set up Echo server
	e := echo.New()
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// Serve static files (Vue SPA)
	e.File("/", "web/public/index.html")
	e.Static("/assets", "web/public/assets")

	// API routes
	apiGroup := e.Group("/api")
	api.SetupRoutes(apiGroup, eng, db)

	// Start server
	e.Logger.Fatal(e.Start(":8080"))
}