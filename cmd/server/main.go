package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gookorbit/internal/api"
	"gookorbit/internal/config"
	"gookorbit/internal/db"
	"gookorbit/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	log := config.Logger(cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		log.Error("migrations", "err", err)
		os.Exit(1)
	}
	pool, q, err := db.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	store, err := storage.New(ctx, cfg.S3, log)
	if err != nil {
		log.Error("storage", "err", err)
		os.Exit(1)
	}
	if err := store.CreateBucketIfMissing(ctx); err != nil {
		log.Error("s3 bucket", "err", err)
		os.Exit(1)
	}

	srv := api.New(cfg, log, q, store)
	httpSrv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	log.Info("gookorbit listening", "port", cfg.Port)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
