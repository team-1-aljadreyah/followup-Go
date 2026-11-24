package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"followapi/internal/app"
	"followapi/internal/config"
	"followapi/internal/dbx"
	"followapi/internal/support"
)

func main() {
	cfg := config.Load(".env")
	db, err := dbx.Open(cfg)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	if db.IsSQLite() {
		if err := db.BootstrapSQLite(context.Background()); err != nil {
			log.Fatalf("sqlite bootstrap failed: %v", err)
		}
	}

	application := app.New(cfg, db)
	application.Push.StartWorker()
	defer application.Push.StopWorker()
	defer application.WS.Close()

	// Optional in-process daily reminder scheduler (baghdad 08:00), idempotent via dedup_key.
	stopSched := make(chan struct{})
	if strings.EqualFold(os.Getenv("ENABLE_DAILY_SCHEDULER"), "true") || os.Getenv("ENABLE_DAILY_SCHEDULER") == "1" {
		go func() {
			ticker := time.NewTicker(15 * time.Minute)
			defer ticker.Stop()
			var lastDay string
			for {
				select {
				case <-stopSched:
					return
				case <-ticker.C:
					nowBaghdad := support.BaghdadNow()
					day := nowBaghdad.Format("2006-01-02")
					if nowBaghdad.Hour() >= 8 && day != lastDay {
						stats, rerr := application.Reminders.Send(context.Background(), day)
						if rerr != nil {
							log.Printf("daily reminders error: %v", rerr)
						} else {
							log.Printf("daily reminders %s completed: %v", day, stats)
							lastDay = day
						}
					}
				}
			}
		}()
	}

	addr := ":" + cfg.Port
	srv := &http.Server{
		Addr:              addr,
		Handler:           application.Router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("%s Go server listening on %s (driver=%s, swagger=%v)", cfg.AppName, addr, cfg.DBConnection, cfg.SwaggerEnabled)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server error: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	close(stopSched)

	log.Println("shutting down gracefully...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
