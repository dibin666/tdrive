// Command tdrive-plugin-builder fetches and compiles a plugin source tree.
//
// The builder is intentionally a separate binary so the main tdrive image can
// remain distroless. It should be bound to a private Unix socket or loopback
// address and never exposed through the public HTTP listener.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dibin/tdrive/internal/config"
	plugin "github.com/dibin/tdrive/internal/plugin"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "tdrive-plugin-builder: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	socket := flag.String("socket", os.Getenv("TDRIVE_PLUGIN_BUILDER_ADDRESS"), "Unix socket or loopback address")
	outputRoot := flag.String("output-root", os.Getenv("TDRIVE_PLUGIN_DIR"), "shared output directory")
	maxSourceSize := flag.String("max-source-bytes", "512MiB", "maximum fetched source bytes")
	buildTimeout := flag.Duration("build-timeout", 10*time.Minute, "maximum duration of one build")
	goCommand := flag.String("go", "go", "Go compiler command")
	flag.Parse()

	if *socket == "" {
		return errors.New("-socket is required")
	}
	if *outputRoot == "" {
		return errors.New("-output-root is required")
	}
	maxSourceBytes, err := config.ParseSize(*maxSourceSize)
	if err != nil || maxSourceBytes <= 0 {
		return fmt.Errorf("invalid -max-source-bytes %q", *maxSourceSize)
	}
	if err := os.MkdirAll(*outputRoot, 0o750); err != nil {
		return fmt.Errorf("create output root: %w", err)
	}

	listener, cleanup, err := listen(*socket)
	if err != nil {
		return err
	}
	defer cleanup()

	builder := &plugin.SourceBuilder{
		MaxSourceBytes: maxSourceBytes,
		BuildTimeout:   *buildTimeout,
		OutputRoot:     *outputRoot,
		GoCommand:      *goCommand,
	}
	server := &http.Server{
		Handler:           plugin.BuilderHandler(builder),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	}
}

func listen(address string) (net.Listener, func(), error) {
	if strings.HasPrefix(address, "unix://") {
		address = strings.TrimPrefix(address, "unix://")
	}
	if strings.HasPrefix(address, "/") || !strings.Contains(address, "://") {
		if directory := filepath.Dir(address); directory != "." {
			if err := os.MkdirAll(directory, 0o750); err != nil {
				return nil, func() {}, fmt.Errorf("create builder socket directory: %w", err)
			}
		}
		if connection, err := net.DialTimeout("unix", address, 100*time.Millisecond); err == nil {
			_ = connection.Close()
			return nil, func() {}, fmt.Errorf("builder socket is already in use: %s", address)
		}
		if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
			return nil, func() {}, fmt.Errorf("remove stale builder socket: %w", err)
		}
		listener, err := net.Listen("unix", address)
		if err != nil {
			return nil, func() {}, fmt.Errorf("listen on builder socket: %w", err)
		}
		_ = os.Chmod(address, 0o660)
		return listener, func() { _ = os.Remove(address) }, nil
	}

	if strings.HasPrefix(address, "tcp://") {
		address = strings.TrimPrefix(address, "tcp://")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, func() {}, fmt.Errorf("invalid builder address %q: %w", address, err)
	}
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return nil, func() {}, errors.New("builder TCP address must be loopback")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, func() {}, fmt.Errorf("listen on builder address: %w", err)
	}
	return listener, func() {}, nil
}
