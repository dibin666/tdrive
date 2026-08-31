package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dibin/tdrive/internal/config"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

// manifestMaxBytes bounds the manifest document. A manifest is a few hundred
// bytes of JSON; anything near this limit is a misconfigured URL rather than a
// plugin.
const manifestMaxBytes = 1 << 20

// releaseFetcher is the network side of installation. It is an interface so
// the manager's tests can install without a public HTTPS host.
type releaseFetcher interface {
	// Manifest returns the decoded manifest and the SHA-256 of the exact bytes
	// it was decoded from, which is what a store index pins.
	Manifest(ctx context.Context, manifestURL string) (tdriveplugin.Manifest, string, error)
	// Download writes the artifact to destPath and returns its SHA-256. It
	// leaves nothing behind when the content does not match the declared
	// digest.
	Download(ctx context.Context, artifact tdriveplugin.Artifact, destPath string) (string, error)
}

// executableName returns the file name a plugin executable is stored under on
// the given GOOS.
//
// Windows must have the .exe suffix, and not merely by convention. os/exec
// resolves an absolute path through lookExtensions, which delegates to
// findExecutable; that function only stats the path as given when it already
// has an extension. For an extension-less path it tries the path plus each
// PATHEXT entry and never stats the bare file at all, so exec.Command sets
// cmd.Err = ErrNotFound and Start fails before CreateProcess is reached — even
// though the file exists and is a valid executable image.
//
// goos is a parameter rather than a direct runtime.GOOS read so the naming
// rule can be tested from any build platform.
func executableName(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// urlValidator decides whether the fetcher may request a URL. Production
// always uses ValidateDownloadURL; the package tests substitute a permissive
// one so they can serve a loopback test server, which ValidateDownloadURL
// correctly refuses.
type urlValidator func(string) (*url.URL, error)

// httpFetcher downloads manifests and plugin binaries over HTTPS. Every URL it
// touches — including each redirect hop — goes through the validator, so a
// plugin author cannot use the installer to reach the deployment's private
// network.
type httpFetcher struct {
	client   *http.Client
	validate urlValidator
	maxBytes int64
}

func newHTTPFetcher(maxBytes int64) *httpFetcher {
	if maxBytes <= 0 {
		maxBytes = config.DefaultPluginBinaryLimit
	}
	return newFetcher(ValidateDownloadURL, maxBytes, 10*time.Minute)
}

func newFetcher(validate urlValidator, maxBytes int64, timeout time.Duration) *httpFetcher {
	return &httpFetcher{
		client:   httpsClient(validate, timeout),
		validate: validate,
		maxBytes: maxBytes,
	}
}

// httpsClient builds a client that re-validates every redirect target. The
// installer and the plugin store both fetch attacker-influenced URLs and need
// the same guarantee.
func httpsClient(validate urlValidator, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			if _, err := validate(request.URL.String()); err != nil {
				return fmt.Errorf("unsafe redirect: %w", err)
			}
			return nil
		},
	}
}

func (fetcher *httpFetcher) Manifest(ctx context.Context, manifestURL string) (tdriveplugin.Manifest, string, error) {
	body, err := fetcher.get(ctx, manifestURL, manifestMaxBytes)
	if err != nil {
		return tdriveplugin.Manifest{}, "", fmt.Errorf("fetch plugin manifest: %w", err)
	}
	manifest, err := tdriveplugin.ParseManifest(body)
	if err != nil {
		return tdriveplugin.Manifest{}, "", err
	}
	digest := sha256.Sum256(body)
	return manifest, hex.EncodeToString(digest[:]), nil
}

func (fetcher *httpFetcher) Download(ctx context.Context, artifact tdriveplugin.Artifact, destPath string) (string, error) {
	response, err := fetcher.do(ctx, artifact.URL)
	if err != nil {
		return "", fmt.Errorf("download plugin binary: %w", err)
	}
	defer response.Body.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return "", fmt.Errorf("create plugin staging directory: %w", err)
	}
	file, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create plugin staging file: %w", err)
	}
	hasher := sha256.New()
	// The limit is one byte over the maximum so a body that is exactly at the
	// limit is accepted and one byte past it is reported rather than silently
	// truncated into a digest mismatch.
	written, copyErr := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(response.Body, fetcher.maxBytes+1))
	closeErr := file.Close()
	discard := func(reason error) (string, error) {
		_ = os.Remove(destPath)
		return "", reason
	}
	if copyErr != nil {
		return discard(fmt.Errorf("download plugin binary: %w", copyErr))
	}
	if closeErr != nil {
		return discard(fmt.Errorf("write plugin binary: %w", closeErr))
	}
	if written > fetcher.maxBytes {
		return discard(fmt.Errorf("plugin binary exceeds the %d byte limit", fetcher.maxBytes))
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(digest, artifact.SHA256) {
		return discard(fmt.Errorf("plugin binary digest %s does not match the manifest digest %s", digest, artifact.SHA256))
	}
	if err := os.Chmod(destPath, 0o750); err != nil {
		return discard(fmt.Errorf("make plugin executable: %w", err))
	}
	return digest, nil
}

func (fetcher *httpFetcher) get(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	response, err := fetcher.do(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response exceeds the %d byte limit", maxBytes)
	}
	return body, nil
}

func (fetcher *httpFetcher) do(ctx context.Context, rawURL string) (*http.Response, error) {
	if _, err := fetcher.validate(rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := fetcher.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		response.Body.Close()
		return nil, fmt.Errorf("returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

// ValidateDownloadURL accepts public HTTPS URLs only. Local paths, plain HTTP,
// embedded credentials, and obvious private-network destinations are rejected
// before any request is made, because the installer fetches URLs that come
// from a plugin manifest or store index rather than from tdrive itself.
func ValidateDownloadURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("plugin URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("plugin URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return nil, errors.New("plugin URL must not contain credentials")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return nil, errors.New("plugin URL host is not allowed")
	}
	if ip := net.ParseIP(hostname); ip != nil && isPrivateAddress(ip) {
		return nil, errors.New("plugin URL host is not allowed")
	}
	return parsed, nil
}

func isPrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
