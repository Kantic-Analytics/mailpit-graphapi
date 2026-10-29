package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/graphapi"
	"github.com/Kantic-Analytics/mailpit-graphapi/internal/loopback"
	"github.com/Kantic-Analytics/mailpit-graphapi/internal/mailpit"
)

var version = "dev"

func main() {
	listen := flag.String("listen", env("MAILPIT_GRAPH_LISTEN", "127.0.0.1:8081"), "HTTP listen address")
	mailpitURL := flag.String("mailpit-url", env("MAILPIT_URL", "http://127.0.0.1:8025"), "Mailpit HTTP URL")
	token := flag.String("token", env("MAILPIT_GRAPH_TOKEN", "mailpit-graphapi-local"), "local bearer token")
	clientID := flag.String("client-id", env("MAILPIT_GRAPH_CLIENT_ID", ""), "optional OAuth client ID")
	clientSecret := flag.String("client-secret", env("MAILPIT_GRAPH_CLIENT_SECRET", ""), "optional OAuth client secret")
	folders := flag.String("folders", env("MAILPIT_GRAPH_FOLDERS", ""), "comma-separated custom mail folders")
	pollInterval := flag.Duration("notification-poll-interval", envDuration("MAILPIT_GRAPH_NOTIFICATION_POLL_INTERVAL", time.Second), "Mailpit polling interval while subscriptions are active")
	allowRemoteHooks := flag.Bool("allow-remote-notification-urls", envBool("MAILPIT_GRAPH_ALLOW_REMOTE_NOTIFICATION_URLS"), "allow non-loopback subscription notification URLs (needed for containers)")
	allowRemote := flag.Bool("allow-remote-mailpit", false, "allow a non-loopback Mailpit URL (needed for containers)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}
	if *token == "" {
		fatal("MAILPIT_GRAPH_TOKEN must not be empty")
	}
	if *pollInterval <= 0 {
		fatal("notification poll interval must be positive")
	}
	if !*allowRemote && !loopback.IsHTTPURL(*mailpitURL) {
		fatal("Mailpit URL must be loopback; pass --allow-remote-mailpit explicitly for an isolated container network")
	}

	mp, err := mailpit.New(*mailpitURL, nil)
	if err != nil {
		fatal(err.Error())
	}
	reconcileCtx, cancelReconcile := context.WithTimeout(context.Background(), 30*time.Second)
	updated, reconcileErr := graphapi.ReconcileDisplayTags(reconcileCtx, mp)
	cancelReconcile()
	if reconcileErr != nil {
		slog.Warn("Mailpit display-tag reconciliation failed", "error", reconcileErr)
	} else {
		slog.Info("Mailpit display tags reconciled", "messages_updated", updated)
	}
	server := graphapi.New(mp, graphapi.Config{
		Token: *token, ClientID: *clientID, ClientSecret: *clientSecret, Folders: splitCSV(*folders),
		NotificationPollInterval: *pollInterval, AllowRemoteNotificationURLs: *allowRemoteHooks,
	})
	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go server.RunNotifications(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("mailpit-graphapi started", "version", version, "listen", *listen, "mailpit", *mailpitURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(err.Error())
	}
}

func env(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		fatal(name + " must be a Go duration such as 1s")
	}
	return duration
}

func envBool(name string) bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		fatal(name + " must be true or false")
	}
	return enabled
}

func splitCSV(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func fatal(message string) {
	slog.Error(message)
	os.Exit(1)
}
