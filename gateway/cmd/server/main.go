// Command server runs the Anuma 2API gateway.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"anuma2api/gateway/internal/api"
	"anuma2api/gateway/internal/config"
	"anuma2api/gateway/internal/pool"
	"anuma2api/gateway/internal/upstream"
)

func main() {
	cfg := config.Load()

	client := upstream.NewClient(cfg.UpstreamBaseURL, cfg.PrivyBaseURL, cfg.ModelCacheTTL)
	// Accounts now arrive via POST /api/accounts; the pool is loaded from
	// SQLite only (SPEC-upload §2.3) — accounts.csv is no longer polled.
	accPool, err := pool.New(client, cfg.DBPath, pool.Options{
		MaxRetries:       cfg.MaxRetries,
		AuthFailLimit:    cfg.AuthFailLimit,
		RefreshInterval:  cfg.RefreshInterval,
		CooldownInterval: cfg.CooldownInterval,
		RetryBackoff:     cfg.RetryBackoff,
	})
	if err != nil {
		log.Printf("FATAL: cannot load accounts pool: %v", err)
		os.Exit(1)
	}
	accPool.Start()
	defer accPool.Close()

	// Auto-continuation (ticket 13): on by default so length-truncated answers
	// are replayed automatically; ANUMA_AUTO_CONTINUE=false or
	// ANUMA_AUTO_CONTINUE_MAX_SEGMENTS<=0 disables it.
	srv := api.New(client, accPool, cfg.AdminPassword, cfg.ModelAllowlist,
		api.WithAutoContinue(cfg.AutoContinue, cfg.AutoContinueMaxSegments))
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      300 * time.Second, // SSE streams can run long (2-8)
		IdleTimeout:       120 * time.Second, // drop idle keep-alive conns (2-8)
	}

	// Graceful shutdown on SIGINT/SIGTERM. HTTP requests are drained BEFORE the
	// pool is closed (accPool.Close is deferred below, so it runs after Shutdown
	// returns): in-flight requests can still deduct credits until the drain
	// window ends (2-4).
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Println("shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	log.Printf("anuma2api gateway listening on %s (db=%s, accounts loaded from SQLite)", httpServer.Addr, cfg.DBPath)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("FATAL: server error: %v", err)
		os.Exit(1)
	}
}
