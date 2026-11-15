// Command ukai serves a small web page that downloads videos and audio with
// yt-dlp.
//
//	ukai              run the server (configured through UKAI_* variables)
//	ukai healthcheck  exit 0 if the local server answers /healthz
//	ukai version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // TZ works in images without zoneinfo

	"github.com/dor1a/ukai/internal/auth"
	"github.com/dor1a/ukai/internal/config"
	"github.com/dor1a/ukai/internal/jobs"
	"github.com/dor1a/ukai/internal/store"
	"github.com/dor1a/ukai/internal/subs"
	"github.com/dor1a/ukai/internal/web"
	"github.com/dor1a/ukai/internal/ytdlp"
)

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "version", "-version", "--version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (use serve, healthcheck, or version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ukai:", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// A missing download directory usually means a missing volume mount.
	// Refuse to start rather than fill the container's own disk.
	if info, err := os.Stat(cfg.DownloadDir); err != nil || !info.IsDir() {
		return fmt.Errorf("UKAI_DOWNLOAD_DIR %s is not a directory", cfg.DownloadDir)
	}
	bin, err := exec.LookPath(cfg.YtDlp)
	if err != nil {
		return fmt.Errorf("yt-dlp not found at %s (UKAI_YTDLP): %w", cfg.YtDlp, err)
	}

	st, err := store.OpenJSON(cfg.DataDir)
	if err != nil {
		return err
	}
	runner := ytdlp.Runner{Bin: bin}
	mgr, err := jobs.New(jobs.Options{
		Store:       st,
		Runner:      runner,
		Workers:     cfg.Concurrency,
		History:     cfg.JobHistory,
		DownloadDir: cfg.DownloadDir,
		TempRoot:    filepath.Join(cfg.DataDir, "tmp"),
		ArchiveDir:  filepath.Join(cfg.DataDir, "archive"),
		Log:         log,
	})
	if err != nil {
		return err
	}
	subscriptions, err := subs.New(subs.Options{
		Store:      st,
		Lister:     runner,
		Queue:      mgr,
		ArchiveDir: filepath.Join(cfg.DataDir, "archive"),
		Interval:   cfg.SubsInterval,
		Log:        log,
	})
	if err != nil {
		return err
	}
	site, err := web.New(mgr, runner, cfg.DownloadDir, log, version)
	if err != nil {
		return err
	}
	site.UseSubs(subscriptions, cfg.SubsInterval)
	if len(cfg.APITokens) > 0 {
		tokens := make([]web.APIToken, len(cfg.APITokens))
		names := make([]string, len(cfg.APITokens))
		for i, t := range cfg.APITokens {
			tokens[i], names[i] = web.APIToken{Name: t.Name, Token: t.Token}, t.Name
		}
		site.UseAPI(tokens)
		log.Info("api on", "tokens", strings.Join(names, ","))
	}
	if cfg.Auth == config.AuthOIDC {
		o := cfg.OIDC
		site.UseAuth(auth.New(auth.Settings{
			PublicURL:    o.PublicURL,
			Issuer:       o.Issuer,
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Group:        o.Group,
			GroupsClaim:  o.GroupsClaim,
			Scopes:       o.Scopes,
			SessionKey:   o.SessionKey,
		}, log), o.Gravatar)
	}
	mgr.SetOnFinish(subscriptions.JobFinished)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	workers := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(workers)
	}()
	checks := make(chan struct{})
	go func() {
		subscriptions.Run(ctx)
		close(checks)
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           site.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("ukai started", "version", version, "listen", cfg.Listen, "auth", cfg.Auth, "workers", cfg.Concurrency, "subs_interval", cfg.SubsInterval)
	if cfg.Auth == config.AuthOIDC {
		log.Info("sign-in required", "issuer", cfg.OIDC.Issuer, "group", cfg.OIDC.Group, "public_url", cfg.OIDC.PublicURL)
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			stop()
			<-workers
			<-checks
			return err
		}
	}
	log.Info("stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	<-workers
	<-checks
	return nil
}

// healthcheck asks the local server for /healthz.
func healthcheck() error {
	addr := os.Getenv("UKAI_LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
