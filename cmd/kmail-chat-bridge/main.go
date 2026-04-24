// Command kmail-chat-bridge is the Chat Bridge entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): bidirectional
// email ↔ KChat channel integration — share an email to a
// channel, route alerts from aliases like alerts@ into a channel,
// extract tasks from emails, and fan JMAP push events into KChat
// notifications (see docs/JMAP-CONTRACT.md §5.3).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/chatbridge"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/middleware"
)

func main() {
	logger := log.New(os.Stderr, "kmail-chat-bridge ", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config.Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	svc := chatbridge.NewService(chatbridge.Config{
		KChatAPIURL:   cfg.KChatAPIURL,
		KChatAPIToken: cfg.KChatAPIToken,
		StalwartURL:   cfg.StalwartURL,
		Pool:          pool,
		Logger:        logger,
	})

	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		Issuer:         cfg.KChatOIDCIssuer,
		Audience:       cfg.KChatOIDCAudience,
		DevBypassToken: cfg.DevBypassToken,
		Pool:           pool,
		Logger:         logger,
	})
	if err != nil {
		logger.Fatalf("middleware.NewOIDC: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	chatbridge.NewHandlers(svc, logger).Register(mux, authMW)

	srv := &http.Server{
		Addr:              cfg.ChatBridge.Addr,
		Handler:           middleware.RequestLogger(logger)(mux),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	serverErr := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s", cfg.ChatBridge.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			logger.Fatalf("http server: %v", err)
		}
	case sig := <-sigCh:
		logger.Printf("received %s, starting graceful shutdown", sig)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("graceful shutdown: %v", err)
	}
	<-serverErr
}
