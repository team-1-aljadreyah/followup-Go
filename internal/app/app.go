package app

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"followapi/docs"
	"followapi/internal/admin"
	"followapi/internal/advanced"
	"followapi/internal/announcements"
	"followapi/internal/attachments"
	"followapi/internal/audit"
	"followapi/internal/auth"
	"followapi/internal/avatars"
	"followapi/internal/bugs"
	"followapi/internal/celebrations"
	"followapi/internal/chat"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/github"
	"followapi/internal/httpx"
	"followapi/internal/monitoring"
	"followapi/internal/notifications"
	"followapi/internal/performance"
	"followapi/internal/projects"
	"followapi/internal/push"
	"followapi/internal/ratings"
	"followapi/internal/reminders"
	"followapi/internal/reports"
	"followapi/internal/search"
	"followapi/internal/tasks"
	"followapi/internal/trials"
	"followapi/internal/ws"
)

type App struct {
	Config        config.Config
	DB            *dbx.DB
	Router        *chi.Mux
	WS            *ws.Hub
	Audit         *audit.Service
	Advanced      *advanced.Service
	Push          *push.Service
	Notifications *notifications.Service
	Auth          *auth.Service
	Projects      *projects.Service
	Tasks         *tasks.Service
	Bugs          *bugs.Service
	Attachments   *attachments.Service
	Announcements *announcements.Service
	Avatars       *avatars.Service
	Chat          *chat.Service
	Performance   *performance.Service
	Monitoring    *monitoring.Service
	Ratings       *ratings.Service
	Celebrations  *celebrations.Service
	Trials        *trials.Service
	Reports       *reports.Service
	Search        *search.Service
	Github        *github.Service
	Admin         *admin.Service
	Reminders     *reminders.Service
}

func New(cfg config.Config, db *dbx.DB) *App {
	auditSvc := audit.New(db)
	advSvc := advanced.New(db, auditSvc)
	pushSvc := push.New(db, cfg)
	notifSvc := notifications.New(db, advSvc, pushSvc)
	authSvc := auth.New(db, cfg, auditSvc, notifSvc, pushSvc, advSvc)
	projSvc := projects.New(db, auditSvc, notifSvc, advSvc)
	taskSvc := tasks.New(db, notifSvc, advSvc)
	bugSvc := bugs.New(db, notifSvc, advSvc)
	attSvc := attachments.New(db, cfg, advSvc, nil)
	chatSvc := chat.New(db, notifSvc, advSvc, pushSvc, attSvc)
	attSvc.SetChatMemberResolver(chatSvc.ChatMembers)

	annSvc := announcements.New(db, auditSvc, notifSvc, advSvc)
	avSvc := avatars.New(db, cfg, auditSvc)
	perfSvc := performance.New(db, auditSvc, notifSvc, advSvc)
	monSvc := monitoring.New(db, notifSvc, advSvc)
	ratSvc := ratings.New(db, cfg, auditSvc, notifSvc, advSvc)
	celSvc := celebrations.New(db)
	triSvc := trials.New(db)
	repSvc := reports.New(db)
	srchSvc := search.New(db)
	ghSvc := github.New(db, cfg, notifSvc, advSvc)
	admSvc := admin.New(db, cfg, auditSvc, notifSvc, advSvc, pushSvc)
	remSvc := reminders.New(db, notifSvc, advSvc, pushSvc)

	wsHub := ws.New(cfg, authSvc.ValidateToken)
	wsHub.SetAuthInvalidators(authSvc.InvalidateUser, authSvc.InvalidateToken, authSvc.InvalidateSessions)
	authSvc.SetHub(wsHub)
	notifSvc.SetHub(wsHub)
	chatSvc.SetHub(wsHub)
	attSvc.SetHub(wsHub)
	celSvc.SetHub(wsHub)
	advSvc.SetHub(wsHub)
	admSvc.SetHub(wsHub)

	a := &App{
		Config:        cfg,
		DB:            db,
		WS:            wsHub,
		Audit:         auditSvc,
		Advanced:      advSvc,
		Push:          pushSvc,
		Notifications: notifSvc,
		Auth:          authSvc,
		Projects:      projSvc,
		Tasks:         taskSvc,
		Bugs:          bugSvc,
		Attachments:   attSvc,
		Announcements: annSvc,
		Avatars:       avSvc,
		Chat:          chatSvc,
		Performance:   perfSvc,
		Monitoring:    monSvc,
		Ratings:       ratSvc,
		Celebrations:  celSvc,
		Trials:        triSvc,
		Reports:       repSvc,
		Search:        srchSvc,
		Github:        ghSvc,
		Admin:         admSvc,
		Reminders:     remSvc,
	}
	a.Router = a.buildRouter()
	return a
}

func (a *App) buildRouter() *chi.Mux {
	r := chi.NewRouter()
	r.Use(httpx.Recoverer)
	r.Use(httpx.Gzip)
	r.Use(httpx.RequestContext(a.Config))
	r.Use(httpx.CORS(a.Config))

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			httpx.HandleErr(w, req, httpx.ErrNotFound())
			return
		}
		http.NotFound(w, req)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			httpx.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"success": false,
				"message": "Method not allowed.",
				"code":    "method_not_allowed",
			})
			return
		}
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	})

	r.Get("/up", func(w http.ResponseWriter, req *http.Request) {
		if err := a.DB.PingContext(req.Context()); err != nil {
			httpx.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		docsLink := ""
		if a.Config.SwaggerEnabled {
			docsLink = `<a href="/docs" style="display:inline-block;margin-top:16px;padding:10px 18px;background:#111827;color:#fff;border-radius:8px;text-decoration:none;font-weight:600">Swagger API Docs</a>`
		}
		_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title></head><body style="font-family:system-ui,-apple-system,sans-serif;background:#f8fafc;color:#0f172a;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0"><main style="background:#fff;padding:36px 40px;border-radius:16px;box-shadow:0 4px 24px rgba(15,23,42,.08);max-width:480px;width:100%%"><h1 style="margin:0 0 8px;font-size:22px">%s API (Go)</h1><p style="margin:0;color:#475569;font-size:14px">FollowUP backend service is running on <code>/api/v1</code>.</p>%s</main></body></html>`,
			html.EscapeString(a.Config.AppName), html.EscapeString(a.Config.AppName), docsLink)
	})

	if a.Config.SwaggerEnabled {
		r.Get("/docs", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8" /><meta name="viewport" content="width=device-width, initial-scale=1.0" /><title>%s — Swagger API Docs</title><link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css" /><style>html,body{margin:0;padding:0;background:#fafafa;}</style></head><body><div id="swagger-ui"></div><script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script><script>window.onload=function(){window.ui=SwaggerUIBundle({url:"/docs/openapi.yaml",dom_id:'#swagger-ui',deepLinking:true,persistAuthorization:true,displayRequestDuration:true,tryItOutEnabled:true,presets:[SwaggerUIBundle.presets.apis]});};</script></body></html>`,
				html.EscapeString(a.Config.AppName))
		})
		r.Get("/docs/openapi.yaml", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(docs.OpenAPIYAML)
		})
	}

	loginLimiter := httpx.NewRateLimiter(a.Config.RateLimitLoginPerMin, time.Minute)
	apiLimiter := httpx.NewRateLimiter(a.Config.RateLimitAPIPerMin, time.Minute)

	r.Route("/api/v1", func(v1 chi.Router) {
		// Public routes
		v1.With(loginLimiter.Middleware(func(req *http.Request) string {
			if ip := httpx.ClientIPFrom(req.Context()); ip != nil {
				return "login:" + *ip
			}
			return "login:" + req.RemoteAddr
		})).Post("/auth/login", a.Auth.HandleLogin)

		v1.Get("/avatars/{user:[0-9]+}/{file}", a.Avatars.HandleShow)
		v1.Get("/attachments/{attachment:[0-9]+}/download", a.Attachments.HandleDownload)
		v1.Post("/webhooks/github", a.Github.HandleWebhook)
		v1.Get("/ws", a.WS.HandleWS)

		// Protected API routes
		v1.Group(func(p chi.Router) {
			p.Use(a.Auth.Middleware)
			p.Use(apiLimiter.Middleware(func(req *http.Request) string {
				if u := httpx.UserFrom(req.Context()); u != nil {
					return fmt.Sprintf("user:%d", u.ID)
				}
				return req.RemoteAddr
			}))
			p.Use(a.Advanced.ReadOnlyMiddleware)

			// Auth & account
			p.Post("/auth/logout", a.Auth.HandleLogout)
			p.Get("/me", a.Auth.HandleMe)
			p.Get("/auth/me", a.Auth.HandleMe)
			p.Put("/me/password", a.Auth.HandleChangePassword)
			p.Post("/auth/change-password", a.Auth.HandleChangePassword)
			p.Get("/users", a.Auth.HandleUsers)
			p.Get("/teams", a.Admin.HandleTeams)

			// Avatars
			p.Put("/me/avatar", a.Avatars.HandleUpdate)
			p.Post("/me/avatar", a.Avatars.HandleUpdate)
			p.Delete("/me/avatar", a.Avatars.HandleDestroyOwn)
			p.Delete("/users/{id:[0-9]+}/avatar", a.Avatars.HandleDestroyUser)

			// Push subscriptions
			p.Post("/me/push-subscriptions", a.Push.HandleSubscribe)
			p.Delete("/me/push-subscriptions", a.Push.HandleUnsubscribe)

			// Settings
			p.Get("/site/live", a.Advanced.HandleLive)
			p.Get("/settings/live", a.Advanced.HandleLive)

			// Projects, sprints, templates, GitHub settings
			p.Get("/projects", a.Projects.HandleIndex)
			p.Post("/projects", a.Projects.HandleCreate)
			p.Get("/projects/{id:[0-9]+}", a.Projects.HandleShow)
			p.Patch("/projects/{id:[0-9]+}", a.Projects.HandleUpdate)
			p.Put("/projects/{id:[0-9]+}/statuses", a.Projects.HandleSaveStatuses)
			p.Put("/projects/{id:[0-9]+}/phases", a.Projects.HandleSavePhases)
			p.Get("/projects/{id:[0-9]+}/history", a.Projects.HandleHistory)
			p.Get("/projects/{id:[0-9]+}/sprints", a.Projects.HandleSprints)
			p.Post("/projects/{id:[0-9]+}/sprints", a.Projects.HandleCreateSprint)
			p.Patch("/sprints/{id:[0-9]+}", a.Projects.HandleUpdateSprint)
			p.Delete("/sprints/{id:[0-9]+}", a.Projects.HandleDeleteSprint)
			p.Post("/sprints/{id:[0-9]+}/close", a.Projects.HandleCloseSprint)
			p.Get("/templates", a.Projects.HandleTemplates)
			p.Post("/projects/{id:[0-9]+}/template", a.Projects.HandleSaveTemplate)
			p.Post("/templates", a.Projects.HandleSaveTemplate)
			p.Delete("/templates/{id:[0-9]+}", a.Projects.HandleDeleteTemplate)
			p.Get("/projects/{id:[0-9]+}/github", a.Github.HandleProjectSettings)
			p.Put("/projects/{id:[0-9]+}/github", a.Github.HandleSaveProjectSettings)

			// Tasks
			p.Get("/projects/{id:[0-9]+}/tasks", a.Tasks.HandleListForProject)
			p.Post("/projects/{id:[0-9]+}/tasks", a.Tasks.HandleCreate)
			p.Get("/me/tasks", a.Tasks.HandleMyTasks)
			p.Get("/tasks/{id:[0-9]+}", a.Tasks.HandleShow)
			p.Patch("/tasks/{id:[0-9]+}", a.Tasks.HandleUpdate)
			p.Post("/tasks/{id:[0-9]+}/status", a.Tasks.HandleMove)
			p.Patch("/tasks/{id:[0-9]+}/status", a.Tasks.HandleMove)
			p.Post("/tasks/{id:[0-9]+}/approval", a.Tasks.HandleDecideApproval)
			p.Put("/tasks/{id:[0-9]+}/dependencies", a.Tasks.HandleSetDependencies)
			p.Get("/tasks/{id:[0-9]+}/comments", a.Tasks.HandleComments)
			p.Post("/tasks/{id:[0-9]+}/comments", a.Tasks.HandleAddComment)
			p.Get("/tasks/{id:[0-9]+}/history", a.Tasks.HandleHistory)
			p.Get("/tasks/{id:[0-9]+}/github", a.Github.HandleTaskItems)
			p.Get("/tasks/{id:[0-9]+}/attachments", a.Attachments.HandleList("task"))
			p.Post("/tasks/{id:[0-9]+}/attachments", a.Attachments.HandleUpload("task"))

			// Bugs
			p.Get("/bugs", a.Bugs.HandleIndex)
			p.Get("/projects/{id:[0-9]+}/bugs", a.Bugs.HandleListForProject)
			p.Post("/projects/{id:[0-9]+}/bugs", a.Bugs.HandleReport)
			p.Get("/bugs/{id:[0-9]+}", a.Bugs.HandleShow)
			p.Patch("/bugs/{id:[0-9]+}", a.Bugs.HandleUpdate)
			p.Post("/bugs/{id:[0-9]+}/status", a.Bugs.HandleMove)
			p.Patch("/bugs/{id:[0-9]+}/status", a.Bugs.HandleMove)
			p.Get("/bugs/{id:[0-9]+}/history", a.Bugs.HandleHistory)
			p.Get("/bugs/{id:[0-9]+}/github", a.Github.HandleBugItems)
			p.Get("/bugs/{id:[0-9]+}/attachments", a.Attachments.HandleList("bug"))
			p.Post("/bugs/{id:[0-9]+}/attachments", a.Attachments.HandleUpload("bug"))

			// Project & Message attachments + delete
			p.Get("/projects/{id:[0-9]+}/attachments", a.Attachments.HandleList("project"))
			p.Post("/projects/{id:[0-9]+}/attachments", a.Attachments.HandleUpload("project"))
			p.Get("/messages/{id:[0-9]+}/attachments", a.Attachments.HandleList("message"))
			p.Post("/messages/{id:[0-9]+}/attachments", a.Attachments.HandleUpload("message"))
			p.Delete("/attachments/{id:[0-9]+}", a.Attachments.HandleDelete)

			// Announcements
			p.Get("/announcements", a.Announcements.HandleIndex)
			p.Post("/announcements", a.Announcements.HandleCreate)
			p.Get("/announcements/{id:[0-9]+}", a.Announcements.HandleShow)
			p.Delete("/announcements/{id:[0-9]+}", a.Announcements.HandleDelete)
			p.Post("/announcements/{id:[0-9]+}/seen", a.Announcements.HandleMarkSeen)
			p.Get("/announcements/{id:[0-9]+}/reads", a.Announcements.HandleReads)

			// Chat
			p.Get("/chats", a.Chat.HandleConversations)
			p.Post("/chats/direct", a.Chat.HandleStartDirect)
			p.Post("/chats/groups", a.Chat.HandleCreateGroup)
			p.Get("/chats/{key}/messages", a.Chat.HandleMessages)
			p.Post("/chats/{key}/messages", a.Chat.HandleSend)
			p.Post("/chats/{key}/read", a.Chat.HandleMarkRead)
			p.Patch("/chats/{key}", a.Chat.HandleUpdateGroup)
			p.Post("/chats/{key}/leave", a.Chat.HandleLeaveGroup)

			// Notifications
			p.Get("/notifications", a.Notifications.HandleIndex)
			p.Post("/notifications/{id:[0-9]+}/read", a.Notifications.HandleMarkRead)
			p.Post("/notifications/read-all", a.Notifications.HandleMarkAllRead)
			p.Get("/notifications/preferences", a.Notifications.HandlePreferences)
			p.Put("/notifications/preferences", a.Notifications.HandleSavePreferences)

			// Search & Reports
			p.Get("/search", a.Search.HandleSearch)
			p.Get("/reports/overview", a.Reports.HandleOverview)

			// Performance & Reviews
			p.Get("/performance", a.Performance.HandleOverview)
			p.Get("/performance/people/{id:[0-9]+}", a.Performance.HandlePerson)
			p.Get("/performance/criteria", a.Performance.HandleCriteria)
			p.Put("/performance/criteria", a.Performance.HandleSaveCriteria)
			p.Post("/performance/criteria/approve", a.Performance.HandleApproveCriteria)
			p.Post("/reviews", a.Performance.HandleCreateReview)
			p.Patch("/reviews/{id:[0-9]+}", a.Performance.HandleUpdateReview)
			p.Post("/reviews/{id:[0-9]+}/finalize", a.Performance.HandleFinalizeReview)

			// Monitoring & CEO Inbox
			p.Get("/monitoring/notes", a.Monitoring.HandleNotes)
			p.Post("/monitoring/notes", a.Monitoring.HandleCreate)
			p.Post("/monitoring/notes/{id:[0-9]+}/send", a.Monitoring.HandleSend)
			p.Post("/monitoring/notes/{id:[0-9]+}/share", a.Monitoring.HandleShare)
			p.Post("/monitoring/notes/{id:[0-9]+}/seen", a.Monitoring.HandleMarkSeen)
			p.Get("/monitoring/received", a.Monitoring.HandleReceived)
			p.Post("/monitoring/requests/{id:[0-9]+}/forward", a.Monitoring.HandleForwardRequest)
			p.Post("/monitoring/requests/{id:[0-9]+}/answer", a.Monitoring.HandleAnswerRequest)
			p.Get("/monitoring/report-facts", a.Monitoring.HandleReportFacts)
			p.Post("/monitoring/report-facts", a.Monitoring.HandleReportFacts)
			p.Get("/ceo/inbox", a.Monitoring.HandleInbox)
			p.Post("/ceo/inbox/{id:[0-9]+}/answer", a.Monitoring.HandleAnswerInbox)

			// Ratings
			p.Get("/teams/ratings/summary", a.Ratings.HandleTeamSummaries)
			p.Get("/teams/{id:[0-9]+}", a.Ratings.HandleTeamDetail)
			p.Get("/teams/{id:[0-9]+}/ratings", a.Ratings.HandleList("team"))
			p.Post("/teams/{id:[0-9]+}/ratings", a.Ratings.HandleCreate("team"))
			p.Get("/people/ratings/summary", a.Ratings.HandlePeopleSummaries)
			p.Get("/people/{id:[0-9]+}/ratings", a.Ratings.HandleList("person"))
			p.Post("/people/{id:[0-9]+}/ratings", a.Ratings.HandleCreate("person"))
			p.Patch("/ratings/{id:[0-9]+}", a.Ratings.HandleUpdate)
			p.Delete("/ratings/{id:[0-9]+}", a.Ratings.HandleDelete)

			// Celebrations
			p.Post("/celebrations", a.Celebrations.HandleStore)
			p.Get("/me/celebrations", a.Celebrations.HandleMine)
			p.Post("/celebrations/{id:[0-9]+}/seen", a.Celebrations.HandleMarkSeen)

			// Trial teams (monitor only)
			p.Get("/trials", a.Trials.HandleIndex)
			p.Post("/trials", a.Trials.HandleCreateTeam)
			p.Get("/trials/{id:[0-9]+}", a.Trials.HandleDetail)
			p.Patch("/trials/{id:[0-9]+}", a.Trials.HandleUpdateTeam)
			p.Post("/trials/{id:[0-9]+}/members", a.Trials.HandleAddMember)
			p.Patch("/trials/members/{id:[0-9]+}", a.Trials.HandleUpdateMember)
			p.Delete("/trials/members/{id:[0-9]+}", a.Trials.HandleDeleteMember)
			p.Post("/trials/members/{id:[0-9]+}/entries", a.Trials.HandleAddEntry)
			p.Delete("/trials/entries/{id:[0-9]+}", a.Trials.HandleDeleteEntry)

			// Admin, Advanced Settings, Audit
			p.Get("/admin/users", a.Admin.HandleUsers)
			p.Post("/admin/users", a.Admin.HandleCreateUser)
			p.Post("/admin/users/import", a.Admin.HandleImportUsers)
			p.Patch("/admin/users/{id:[0-9]+}", a.Admin.HandleUpdateUser)
			p.Post("/admin/users/{id:[0-9]+}/unlock", a.Admin.HandleUnlock)
			p.Get("/admin/sessions", a.Admin.HandleSessions)
			p.Delete("/admin/sessions/{id}", a.Admin.HandleRevokeSession)
			p.Delete("/admin/users/{id:[0-9]+}/sessions", a.Admin.HandleRevokeUserSessions)
			p.Get("/admin/security/summary", a.Admin.HandleSecuritySummary)
			p.Post("/admin/teams", a.Admin.HandleCreateTeam)
			p.Patch("/admin/teams/{id:[0-9]+}", a.Admin.HandleUpdateTeam)
			p.Get("/admin/settings", a.Admin.HandleSettings)
			p.Put("/admin/settings", a.Admin.HandleSaveSettings)
			p.Get("/admin/health", a.Admin.HandleHealth)
			p.Get("/ceo/advanced", a.Advanced.HandleGet)
			p.Put("/ceo/advanced", a.Advanced.HandleSave)
			p.Get("/ceo/advanced/history", a.Advanced.HandleHistory)
			p.Get("/admin/advanced", a.Advanced.HandleGet)
			p.Put("/admin/advanced", a.Advanced.HandleSave)
			p.Get("/admin/advanced/history", a.Advanced.HandleHistory)
			p.Get("/audit", a.Admin.HandleAudit)
			p.Get("/audit/{id:[0-9]+}", a.Admin.HandleAuditDetail)
		})
	})

	return r
}
