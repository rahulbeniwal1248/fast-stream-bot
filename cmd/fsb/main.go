package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/biisal/fast-stream-bot/config"
	"github.com/biisal/fast-stream-bot/internal/bot"
	db "github.com/biisal/fast-stream-bot/internal/database/sqlite"
	repo "github.com/biisal/fast-stream-bot/internal/database/sqlite/sqlc"
	"github.com/biisal/fast-stream-bot/internal/http-server/routers"
	"github.com/biisal/fast-stream-bot/internal/http-server/shortner"
	redisstore "github.com/biisal/fast-stream-bot/internal/redis"
	"github.com/biisal/fast-stream-bot/internal/service/user"
	"github.com/biisal/fast-stream-bot/logger"
)

const (
	defaultDBString       = "fsb.db"
	defaultRedisAddr      = "127.0.0.1:6379"
	defaultHTTPPort       = 8000
	defaultJWTExpiration  = 24 * 60 * 60
	defaultUUIDExpiration = 10 * 60
	defaultCacheTTL       = 30 * time.Minute
)

func main() {
	printLogo("")
	cfg := config.MustLoad("")

	logCloser, err := logger.SetupSlog(cfg.ENVIRONMENT, logger.HandlerMulti)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to setup logger: %v\n", err)
		os.Exit(1)
	}
	if logCloser != nil {
		defer logCloser.Close()
	}

	if err := applyRuntimeDefaults(&cfg); err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sqlDB, err := db.CreateConn(ctx, cfg.DBSTRING)
	if err != nil {
		slog.Error("failed to connect database", "error", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	redisClient, redisService, err := redisstore.New(ctx, cfg.REDIS_CONFIG.ADDRESS)
	if err != nil {
		slog.Error("failed to connect redis", "address", cfg.REDIS_CONFIG.ADDRESS, "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	userService := user.NewService(repo.New(sqlDB), redisService, defaultCacheTTL)
	worker := bot.StartWorkers(&cfg, userService)
	shortnerSvc := shortner.NewShortner(
		time.Duration(cfg.JWT_EXPIRATION)*time.Second,
		time.Duration(cfg.UUID_EXPIRATION)*time.Second,
		cfg.JWT_SECRET,
		redisService,
		cfg.SHORTNER_URL,
		cfg.SHORTNER_API,
		cfg,
	)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.HTTP_PORT),
		Handler:           routers.SetUpRouters(worker, cfg, shortnerSvc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("HTTP server started", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown failed", "error", err)
	}
}

func applyRuntimeDefaults(cfg *config.Config) error {
	if strings.TrimSpace(cfg.DBSTRING) == "" {
		cfg.DBSTRING = defaultDBString
	}
	if strings.TrimSpace(cfg.REDIS_CONFIG.ADDRESS) == "" {
		cfg.REDIS_CONFIG.ADDRESS = defaultRedisAddr
	}
	if cfg.HTTP_PORT == 0 {
		cfg.HTTP_PORT = defaultHTTPPort
	}
	if cfg.JWT_EXPIRATION <= 0 {
		cfg.JWT_EXPIRATION = defaultJWTExpiration
	}
	if cfg.UUID_EXPIRATION <= 0 {
		cfg.UUID_EXPIRATION = defaultUUIDExpiration
	}
	if len(cfg.JWT_SECRET) == 0 {
		cfg.JWT_SECRET = []byte(cfg.APP_HASH)
	}
	if len(cfg.JWT_SECRET) == 0 {
		return fmt.Errorf("JWT_SECRET or APP_HASH is required")
	}
	if len(cfg.BOT_TOKENS) == 0 {
		return fmt.Errorf("BOT_TOKENS is required")
	}
	if cfg.APP_HASH == "" {
		return fmt.Errorf("APP_HASH is required")
	}
	if cfg.DB_CHANNEL_ID == 0 {
		return fmt.Errorf("DB_CHANNEL_ID is required")
	}
	return nil
}
