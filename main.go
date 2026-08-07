package main

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"shopping-list/api"
	"shopping-list/db"
	"shopping-list/handlers"
	"shopping-list/i18n"
	"shopping-list/webhook"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/compress"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/template/html/v2"
	"github.com/gofiber/websocket/v2"
)

//go:embed templates/*
var embeddedTemplatesFS embed.FS

//go:embed static/*
var embeddedStaticFS embed.FS

func main() {
	// Initialize i18n first (before db, so migrations can use translations)
	if err := i18n.Init(); err != nil {
		log.Fatal("Failed to initialize i18n:", err)
	}

	// Set default language from env var (if specified)
	if lang := os.Getenv("DEFAULT_LANG"); lang != "" {
		i18n.SetDefaultLang(lang)
	}

	// Determine the URL mount prefix (for hosting under a subfolder behind a proxy)
	handlers.InitBasePath()
	if handlers.BasePath != "" {
		log.Printf("Mounting app under base path: %s", handlers.BasePath)
	}

	// Initialize database
	db.Init()
	defer db.Close()

	if err := webhook.ConfigureFromEnv(db.DB); err != nil {
		log.Printf("Outbound webhooks are disabled: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := webhook.Shutdown(ctx); err != nil {
			log.Printf("Webhook worker shutdown timed out: %v", err)
		}
	}()

	// Clean expired sessions on startup
	db.CleanExpiredSessions()

	// Initialize login rate limiter
	handlers.InitLoginRateLimiter()

	// Initialize template engine
	templatesRootFS, err := fs.Sub(embeddedTemplatesFS, "templates")
	if err != nil {
		log.Fatalf("Embedded templates directory missing: %v", err)
	}

	engine := html.NewFileSystem(http.FS(templatesRootFS), ".html")
	engine.Reload(os.Getenv("APP_ENV") != "production")

	// Add custom template functions
	engine.AddFuncMap(template.FuncMap{
		"dict": func(values ...interface{}) map[string]interface{} {
			if len(values)%2 != 0 {
				return nil
			}
			dict := make(map[string]interface{}, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					continue
				}
				dict[key] = values[i+1]
			}
			return dict
		},
		"add": func(a, b int) int {
			return a + b
		},
		"sub": func(a, b int) int {
			return a - b
		},
		"mul": func(a, b int) int {
			return a * b
		},
		"div": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"gt": func(a, b int) bool {
			return a > b
		},
		"lt": func(a, b int) bool {
			return a < b
		},
		"eq": func(a, b interface{}) bool {
			return a == b
		},
		"ne": func(a, b interface{}) bool {
			return a != b
		},
		// i18n functions
		"T": i18n.T,
		"toJSON": func(v interface{}) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("{}")
			}
			return template.JS(b)
		},
		"asset": func(path string) string {
			return handlers.BasePath + "/static/" + path + "?v=" + handlers.AssetHash
		},
		// url prefixes a root-absolute app path with the configured BasePath.
		"url": func(path string) string {
			return handlers.BasePath + path
		},
		// basePath exposes the mount prefix to templates (for window.BASE_PATH).
		"basePath": func() string {
			return handlers.BasePath
		},
	})

	// Initialize Fiber app
	app := fiber.New(fiber.Config{
		Views:       engine,
		ViewsLayout: "layout",
	})

	// Middleware
	app.Use(logger.New())
	app.Use(recover.New())
	app.Use(compress.New(compress.Config{Level: compress.LevelBestSpeed}))

	// Static files
	staticRootFS, err := fs.Sub(embeddedStaticFS, "static")
	if err != nil {
		log.Fatalf("Embedded static directory missing: %v", err)
	}

	// Compute content hash across embedded static FS. Used as ?v=<hash>
	// cache-buster in templates and injected into sw.js placeholders so
	// any change to static/ automatically invalidates browser + SW caches.
	hash, err := handlers.ComputeAssetHash(staticRootFS)
	if err != nil {
		log.Fatalf("Failed to compute asset hash: %v", err)
	}
	handlers.AssetHash = hash
	log.Printf("Asset hash: %s", hash)

	swBytes, err := handlers.BuildServiceWorker(staticRootFS, hash)
	if err != nil {
		log.Fatalf("Failed to build service worker: %v", err)
	}
	handlers.ServiceWorkerBytes = swBytes

	manifestBytes, err := handlers.BuildManifest(staticRootFS)
	if err != nil {
		log.Fatalf("Failed to build manifest: %v", err)
	}
	handlers.ManifestBytes = manifestBytes

	// All routes are mounted under BasePath so the app can be served from a
	// subfolder behind a reverse proxy. When BasePath is empty this is a no-op
	// and everything is served from the root.
	root := app.Group(handlers.BasePath)

	// SW must be served by a dedicated handler (not the filesystem middleware)
	// so placeholders get replaced and Cache-Control is no-cache instead of
	// the 30-day max-age applied to other static assets.
	root.Get("/static/sw.js", handlers.ServeServiceWorker)

	// Manifest is served dynamically so its URLs can be rewritten for BasePath.
	root.Get("/static/manifest.json", handlers.ServeManifest)

	root.Use("/static", filesystem.New(filesystem.Config{
		Root:   http.FS(staticRootFS),
		Browse: false,
		MaxAge: 86400 * 30, // 30 days - files are embedded and versioned at build time
	}))

	// Auth routes (before middleware)
	root.Get("/login", handlers.LoginPage)
	root.Post("/login", handlers.LoginRateLimitMiddleware, handlers.Login)
	root.Post("/logout", handlers.Logout)

	// i18n API (before auth middleware - needed for login page)
	root.Get("/locales", handlers.GetLocales)

	// REST API (before auth middleware - uses token auth)
	api.Register(root)

	// Public endpoints (no auth required)
	root.Get("/api/version", handlers.GetVersion)

	// Auth middleware for all other routes
	root.Use(handlers.AuthMiddleware)

	// WebSocket upgrade middleware
	root.Use("/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	// WebSocket endpoint
	root.Get("/ws", websocket.New(handlers.WebSocketHandler))

	// Main page - shows all lists
	root.Get("/", handlers.GetListsPage)

	// Single list view - shows items
	root.Get("/lists/:id", handlers.GetListView)

	// Sections API
	root.Get("/sections/list", handlers.GetSectionsListForModal)
	root.Get("/sections/:id/html", handlers.GetSectionHTML)
	root.Post("/sections", handlers.CreateSection)
	root.Put("/sections/:id", handlers.UpdateSection)
	root.Delete("/sections/:id", handlers.DeleteSection)
	root.Post("/sections/:id/move-up", handlers.MoveSectionUp)
	root.Post("/sections/:id/move-down", handlers.MoveSectionDown)
	root.Post("/sections/:id/check-all", handlers.CheckAllItems)
	root.Post("/sections/:id/uncheck-all", handlers.UncheckAllItems)
	root.Post("/sections/:id/sort-mode", handlers.UpdateSectionSortMode)

	// Lists API
	root.Get("/lists", handlers.GetLists)
	root.Post("/lists", handlers.CreateList)
	root.Put("/lists/:id", handlers.UpdateList)
	root.Delete("/lists/:id", handlers.DeleteList)
	root.Post("/lists/:id/activate", handlers.SetActiveList)
	root.Get("/lists/:id/activate", handlers.SetActiveList)
	root.Post("/lists/:id/move-up", handlers.MoveListUp)
	root.Post("/lists/:id/move-down", handlers.MoveListDown)
	root.Post("/lists/:id/toggle-completed", handlers.ToggleShowCompleted)

	// Templates API
	root.Get("/templates", handlers.GetTemplates)
	root.Get("/templates/:id", handlers.GetTemplate)
	root.Post("/templates", handlers.CreateTemplate)
	root.Put("/templates/:id", handlers.UpdateTemplate)
	root.Delete("/templates/:id", handlers.DeleteTemplate)
	root.Post("/templates/:id/items", handlers.AddTemplateItem)
	root.Put("/templates/:id/items/:itemId", handlers.UpdateTemplateItem)
	root.Delete("/templates/:id/items/:itemId", handlers.DeleteTemplateItem)
	root.Post("/templates/:id/apply", handlers.ApplyTemplate)
	root.Post("/templates/from-list", handlers.CreateTemplateFromList)

	// Items API
	root.Get("/items/:id/html", handlers.GetItemHTML)
	root.Post("/items", handlers.CreateItem)
	root.Post("/items/delete-completed", handlers.DeleteCompletedItems)
	root.Put("/items/:id", handlers.UpdateItem)
	root.Delete("/items/:id", handlers.DeleteItem)
	root.Post("/items/:id/toggle", handlers.ToggleItem)
	root.Post("/items/:id/quantity", handlers.AdjustItemQuantity)
	root.Post("/items/:id/uncertain", handlers.ToggleUncertain)
	root.Post("/items/:id/move", handlers.MoveItemToSection)
	root.Post("/items/:id/move-up", handlers.MoveItemUp)
	root.Post("/items/:id/move-down", handlers.MoveItemDown)

	// Stats API
	root.Get("/stats", handlers.GetStats)

	// Offline data API
	root.Get("/api/data", handlers.GetAllData)
	root.Get("/api/item/:id/version", handlers.GetItemVersion)
	root.Get("/api/suggestions", handlers.GetSuggestions)

	// History management API
	root.Get("/api/history", handlers.GetHistory)
	root.Delete("/api/history/:id", handlers.DeleteHistoryItem)
	root.Post("/api/history/batch-delete", handlers.BatchDeleteHistory)

	// Batch operations
	root.Post("/sections/batch-delete", handlers.BatchDeleteSections)

	// Import/Export
	root.Get("/export", handlers.ExportAllData)
	root.Get("/export/list/:id", handlers.ExportSingleList)
	root.Get("/export/preview", handlers.GetExportPreview)
	root.Post("/import", handlers.ImportData)
	root.Post("/import/preview", handlers.PreviewImport)

	// Database management
	root.Get("/api/database/csrf-token", handlers.GenerateCSRFToken)
	root.Post("/api/database/clear", handlers.ClearDatabase)

	// Get port from env or default to 3000
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	log.Printf("Starting server on port %s", port)
	if err := app.Listen(":" + port); err != nil {
		log.Printf("Server stopped: %v", err)
	}
}
