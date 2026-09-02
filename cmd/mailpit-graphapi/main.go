package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Kantic-Analytics/mailpit-graphapi/internal/graphapi"
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
	if !*allowRemote && !isLoopbackURL(*mailpitURL) {
		fatal("Mailpit URL must be loopback; pass --allow-remote-mailpit explicitly for an isolated container network")
	}

	mp, err := mailpit.New(*mailpitURL, nil)
	if err != nil {
		fatal(err.Error())
	}
	handler := graphapi.New(mp, graphapi.Config{
		Token: *token, ClientID: *clientID, ClientSecret: *clientSecret, Folders: splitCSV(*folders),
	}).Handler()
	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
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

func splitCSV(raw string) []string {
	var out []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func fatal(message string) {
	slog.Error(message)
	os.Exit(1)
}
