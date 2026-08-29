// Command tdrive serves a Telegram-backed drive over a web UI and WebDAV.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/dibin/tdrive/internal/api"
	"github.com/dibin/tdrive/internal/auth"
	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
	"github.com/dibin/tdrive/internal/dav"
	"github.com/dibin/tdrive/internal/drive"
	"github.com/dibin/tdrive/internal/events"
	"github.com/dibin/tdrive/internal/indexer"
	"github.com/dibin/tdrive/internal/tgc"
	"github.com/dibin/tdrive/ui"
)

// version is set at build time with -ldflags "-X main.version=...". It is
// reported to Telegram as the client app version and shown in the UI.
var version = "0.1"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tdrive: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Signals are handled at the top so an in-flight upload gets a chance to
	// finish its current segment rather than being killed mid-part.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.Server.DataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	db, err := database.Open(ctx, cfg.Storage.DatabaseFile)
	if err != nil {
		return err
	}
	defer db.Close()
	// Values saved through the WebUI override legacy environment fallbacks.
	if err := cfg.ApplyStoredRuntimeSettings(ctx, db); err != nil {
		return fmt.Errorf("load runtime settings: %w", err)
	}

	log, logLevel, err := newLogger(cfg.RuntimeSettings().LogLevel)
	if err != nil {
		return err
	}
	defer log.Sync()

	// Telegram is told the app version, and the UI displays it.
	tgc.Version = version
	log.Info("tdrive starting", zap.String("version", version))
	log.Info("index opened", zap.String("path", db.Path()))

	authSvc, err := auth.New(ctx, cfg, db)
	if err != nil {
		return err
	}
	if err := authSvc.Bootstrap(ctx); err != nil {
		return fmt.Errorf("seed the administrator account: %w", err)
	}

	broker := events.NewBroker()
	tgm := tgc.New(cfg, db, log.Named("telegram"))
	driveSvc := drive.New(cfg, db, tgm, log.Named("drive"))
	idx := indexer.New(db, tgm, log.Named("indexer"))
	api.WireIndexProgress(idx, broker)
	api.WireRemoteProgress(driveSvc, broker)

	// A failure to connect here is not fatal: the WebUI still needs to come
	// up so an administrator can fix the credentials.
	if err := tgm.Start(ctx); err != nil {
		log.Error("could not connect to telegram at startup", zap.Error(err))
	}
	defer tgm.Stop()

	if tgm.Ready() {
		driveSvc.ResumeRemotes(ctx)
	}

	apiServer := api.New(cfg, db, authSvc, driveSvc, tgm, idx, broker, log.Named("api"), func(level string) error {
		parsed, err := zapcore.ParseLevel(level)
		if err != nil {
			return fmt.Errorf("invalid log level %q", level)
		}
		logLevel.SetLevel(parsed)
		return nil
	})

	mux := http.NewServeMux()
	mux.Handle("/api/", http.StripPrefix("/api", apiServer.Routes()))
	davHandler := dav.Handler(cfg, db, driveSvc, authSvc, log.Named("webdav"))
	davHandler = enabledHandler(cfg, davHandler)
	mux.Handle(cfg.WebDAV.Prefix+"/", davHandler)
	// Clients probe the bare mount point before walking into it.
	mux.Handle(cfg.WebDAV.Prefix, davHandler)
	if cfg.RuntimeSettings().WebDAVEnabled {
		log.Info("webdav mounted", zap.String("prefix", cfg.WebDAV.Prefix))
	}
	mux.Handle("/", ui.Handler())

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		// Read and write deadlines are deliberately absent: a single upload
		// segment or a slow download legitimately runs for many minutes, and
		// a blanket timeout would cut them off. Idle connections are still
		// reaped.
		IdleTimeout: 120 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	go startMaintenance(ctx, db, log)

	errCh := make(chan error, 1)
	go func() {
		log.Info("tdrive listening",
			zap.String("addr", cfg.Server.Listen),
			zap.String("segment_size", humanBytes(cfg.RuntimeSettings().SegmentSize)))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// startMaintenance trims the rows that would otherwise grow without bound.
func startMaintenance(ctx context.Context, db *database.DB, log *zap.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()

	sweep := func() {
		if n, err := db.PurgeExpiredTokens(ctx); err != nil {
			log.Warn("could not purge expired sessions", zap.Error(err))
		} else if n > 0 {
			log.Debug("purged expired sessions", zap.Int64("count", n))
		}
		// A week of transfer history is enough to explain what happened
		// without letting the table accumulate forever.
		cutoff := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
		if _, err := db.PurgeFinishedJobs(ctx, cutoff); err != nil {
			log.Warn("could not purge old transfers", zap.Error(err))
		}
	}

	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

func newLogger(level string) (*zap.Logger, *zap.AtomicLevel, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		lvl = zapcore.InfoLevel
	}
	cfg := zap.NewProductionConfig()
	atomicLevel := zap.NewAtomicLevelAt(lvl)
	cfg.Level = atomicLevel
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	// Container logs are read by humans far more often than by a log
	// aggregator, so plain console output beats JSON here.
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	log, err := cfg.Build()
	return log, &atomicLevel, err
}

// enabledHandler keeps the WebDAV mount registered while allowing the admin
// to toggle it from the WebUI without rebuilding the HTTP server.
func enabledHandler(cfg *config.Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !cfg.RuntimeSettings().WebDAVEnabled {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
