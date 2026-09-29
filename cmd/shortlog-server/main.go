package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"shortlog-server/internal/auth"
	"shortlog-server/internal/mail"
	"shortlog-server/internal/notes"
	"shortlog-server/internal/projects"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if err := loadLocalEnv(".env"); err != nil {
		return err
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required (set it in the environment or create a local .env file)")
	}

	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	defer pool.Close()

	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(connectCtx); err != nil {
		return errors.New("cannot connect to database")
	}
	if len(os.Args) > 1 {
		if len(os.Args) == 2 && os.Args[1] == "migrate" {
			return migrate(ctx, pool)
		}
		if len(os.Args) != 2 || os.Args[1] != "purge-expired-accounts" {
			return errors.New("usage: shortlog-server [migrate | purge-expired-accounts]")
		}
		return purgeExpiredAccounts(ctx, auth.New(pool))
	}

	service := auth.New(pool)

	key, err := hex.DecodeString(os.Getenv("EMAIL_OTP_KEY"))
	if err != nil || len(key) != 32 {
		return errors.New("EMAIL_OTP_KEY must be 32 random bytes encoded as 64 hex characters")
	}

	password := os.Getenv("SMTP_PASSWORD")
	var sender auth.CodeSender

	if password != "" {
		sender = mail.SMTP{Address: net.JoinHostPort(os.Getenv("SMTP_HOST"), os.Getenv("SMTP_PORT")),
			Username: os.Getenv("SMTP_USER"), From: os.Getenv("SMTP_FROM"), Password: password}
		if os.Getenv("SMTP_HOST") == "" || os.Getenv("SMTP_PORT") != "465" || os.Getenv("SMTP_USER") == "" || os.Getenv("SMTP_FROM") == "" {
			return errors.New("SMTP_HOST, SMTP_PORT=465, SMTP_USER, and SMTP_FROM are required when SMTP_PASSWORD is set")
		}
	}
	var telegram *auth.TelegramLogin
	clientID, clientSecret := os.Getenv("TELEGRAM_CLIENT_ID"), os.Getenv("TELEGRAM_CLIENT_SECRET")
	redirect := os.Getenv("TELEGRAM_REDIRECT_URI")
	telegramKey := os.Getenv("TELEGRAM_ENCRYPTION_KEY")
	if clientID != "" || clientSecret != "" || redirect != "" || telegramKey != "" {
		key, err := hex.DecodeString(telegramKey)
		callback, parseErr := url.Parse(redirect)
		if err != nil || len(key) != 32 || parseErr != nil || callback.Scheme != "https" || callback.Host == "" || callback.Path != "/v1/auth/telegram/callback" || callback.RawQuery != "" || callback.Fragment != "" {
			return errors.New("Telegram login requires a 32-byte TELEGRAM_ENCRYPTION_KEY and an HTTPS TELEGRAM_REDIRECT_URI ending in /v1/auth/telegram/callback")
		}
		initCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		telegram, err = auth.NewTelegramLogin(initCtx, service, auth.TelegramConfig{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURI: redirect, EncryptionKey: key,
		})
		if err != nil {
			return err
		}
	}

	server := &http.Server{
		Addr:              address,
		Handler:           newHandler(pool, service, auth.NewEmailLogin(service, sender, key), telegram, projects.New(pool), notes.New(pool)),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()
	slog.Info("server listening", "address", address)

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func purgeExpiredAccounts(ctx context.Context, service *auth.Service) error {
	total := 0
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		count, err := service.PurgeExpiredAccounts(batchCtx, 100)
		cancel()
		if err != nil {
			return fmt.Errorf("purge expired accounts after %d: %w", total, err)
		}
		total += count
		if count < 100 {
			break
		}
	}
	slog.Info("expired account purge complete", "accounts", total)
	return nil
}

func loadLocalEnv(path string) error {
	if os.Getenv("DATABASE_URL") != "" || os.Getenv("APP_ENV") == "production" {
		return nil
	}
	values, err := godotenv.Read(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load local environment file: %w", err)
	}
	for key, value := range values {
		if os.Getenv(key) == "" {
			if err := os.Setenv(key, value); err != nil {
				return fmt.Errorf("set local environment variable %s: %w", key, err)
			}
		}
	}
	return nil
}
