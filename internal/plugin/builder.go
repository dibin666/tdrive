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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
	getter "github.com/hashicorp/go-getter/v2"
)

// BuilderRequest is shared by the manager and the standalone builder
// process. Inspection and build both re-fetch the exact source/ref and compare
// SourceDigest, which prevents a mutable branch from changing between the
// confirmation screen and compilation.
type BuilderRequest struct {
	SourceURL            string `json:"sourceUrl"`
	Ref                  string `json:"ref,omitempty"`
	ExpectedSourceDigest string `json:"expectedSourceDigest,omitempty"`
	PluginID             string `json:"pluginId,omitempty"`
	OutputPath           string `json:"outputPath,omitempty"`
	GOOS                 string `json:"goos,omitempty"`
	GOARCH               string `json:"goarch,omitempty"`
}

// BuilderResponse is returned after the source has been checked or compiled.
type BuilderResponse struct {
	Manifest     tdriveplugin.Manifest `json:"manifest"`
	SourceDigest string                `json:"sourceDigest"`
	BinaryDigest string                `json:"binaryDigest,omitempty"`
	OutputPath   string                `json:"outputPath,omitempty"`
}

// SourceBuilder contains the sidecar's actual fetch and compile operations.
// It is kept separate from the HTTP transport so unit tests do not need a
// running socket or a public network.
type SourceBuilder struct {
	MaxSourceBytes int64
	BuildTimeout   time.Duration
	OutputRoot     string
	GoCommand      string
}

// Close makes SourceBuilder usable as an embedded builder in tests and
// standalone deployments. It owns no persistent process; each build has its
// own context-bound compiler process.
func (builder *SourceBuilder) Close() {}

// Inspect fetches a source tree, validates its root manifest, and returns a
// deterministic digest without compiling untrusted code.
func (builder *SourceBuilder) Inspect(ctx context.Context, request BuilderRequest) (BuilderResponse, error) {
	if err := validateSourceRequest(request); err != nil {
		return BuilderResponse{}, err
	}
	sourceDir, cleanup, err := builder.fetch(ctx, request)
	if err != nil {
		return BuilderResponse{}, err
	}
	defer cleanup()

	manifest, err := tdriveplugin.ReadManifest(sourceDir)
	if err != nil {
		return BuilderResponse{}, err
	}
	digest, err := digestSource(sourceDir, builder.MaxSourceBytes)
	if err != nil {
		return BuilderResponse{}, err
	}
	if request.ExpectedSourceDigest != "" && !strings.EqualFold(request.ExpectedSourceDigest, digest) {
		return BuilderResponse{}, fmt.Errorf("source digest changed: expected %s, got %s", request.ExpectedSourceDigest, digest)
	}
	return BuilderResponse{Manifest: manifest, SourceDigest: digest}, nil
}

// Build compiles the declared entrypoint into a target-specific, CGO-free
// executable and returns its digest.
func (builder *SourceBuilder) Build(ctx context.Context, request BuilderRequest) (BuilderResponse, error) {
	if err := validateSourceRequest(request); err != nil {
		return BuilderResponse{}, err
	}
	if request.OutputPath == "" {
		return BuilderResponse{}, errors.New("builder outputPath is required")
	}
	if err := builder.validateOutputPath(request.OutputPath); err != nil {
		return BuilderResponse{}, err
	}

	sourceDir, cleanup, err := builder.fetch(ctx, request)
	if err != nil {
		return BuilderResponse{}, err
	}
	defer cleanup()

	manifest, err := tdriveplugin.ReadManifest(sourceDir)
	if err != nil {
		return BuilderResponse{}, err
	}
	digest, err := digestSource(sourceDir, builder.MaxSourceBytes)
	if err != nil {
		return BuilderResponse{}, err
	}
	if request.ExpectedSourceDigest != "" && !strings.EqualFold(request.ExpectedSourceDigest, digest) {
		return BuilderResponse{}, fmt.Errorf("source digest changed: expected %s, got %s", request.ExpectedSourceDigest, digest)
	}
	if request.PluginID != "" && request.PluginID != manifest.ID {
		return BuilderResponse{}, fmt.Errorf("manifest id %q does not match requested plugin %q", manifest.ID, request.PluginID)
	}

	if err := os.MkdirAll(filepath.Dir(request.OutputPath), 0o750); err != nil {
		return BuilderResponse{}, fmt.Errorf("create builder output directory: %w", err)
	}
	if err := os.Remove(request.OutputPath); err != nil && !os.IsNotExist(err) {
		return BuilderResponse{}, fmt.Errorf("clear builder output: %w", err)
	}

	buildTimeout := builder.BuildTimeout
	if buildTimeout <= 0 {
		buildTimeout = 10 * time.Minute
	}
	buildCtx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	goCommand := builder.GoCommand
	if goCommand == "" {
		goCommand = "go"
	}
	goos := request.GOOS
	if goos == "" {
		goos = "linux"
	}
	goarch := request.GOARCH
	if goarch == "" {
		goarch = "amd64"
	}
	command := exec.CommandContext(buildCtx, goCommand, "build", "-trimpath", "-buildvcs=false",
		"-o", request.OutputPath, manifest.Entrypoint)
	command.Dir = sourceDir
	command.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS="+goos,
		"GOARCH="+goarch,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		if buildCtx.Err() != nil {
			return BuilderResponse{}, fmt.Errorf("build plugin: %w", buildCtx.Err())
		}
		return BuilderResponse{}, fmt.Errorf("build plugin: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if err := os.Chmod(request.OutputPath, 0o750); err != nil {
		return BuilderResponse{}, fmt.Errorf("make plugin executable: %w", err)
	}
	binaryDigest, err := digestFile(request.OutputPath)
	if err != nil {
		return BuilderResponse{}, fmt.Errorf("digest plugin binary: %w", err)
	}
	return BuilderResponse{
		Manifest:     manifest,
		SourceDigest: digest,
		BinaryDigest: binaryDigest,
		OutputPath:   request.OutputPath,
	}, nil
}

func (builder *SourceBuilder) fetch(ctx context.Context, request BuilderRequest) (string, func(), error) {
	sourceRoot, err := os.MkdirTemp("", "tdrive-plugin-source-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create source workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(sourceRoot) }

	client := newSourceGetter(builder.MaxSourceBytes)
	_, err = client.Get(ctx, &getter.Request{
		Src:             sourceWithRef(request.SourceURL, request.Ref),
		Dst:             sourceRoot,
		GetMode:         getter.ModeDir,
		Copy:            true,
		DisableSymlinks: true,
		Umask:           0o077,
	})
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("get plugin source: %w", err)
	}
	return sourceRoot, cleanup, nil
}

func (builder *SourceBuilder) validateOutputPath(outputPath string) error {
	if builder.OutputRoot == "" {
		return nil
	}
	root, err := filepath.Abs(builder.OutputRoot)
	if err != nil {
		return fmt.Errorf("resolve builder output root: %w", err)
	}
	output, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve builder output: %w", err)
	}
	relative, err := filepath.Rel(root, output)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("builder output %q is outside %q", outputPath, builder.OutputRoot)
	}
	return nil
}

func validateSourceRequest(request BuilderRequest) error {
	if _, err := ValidateSourceURL(request.SourceURL); err != nil {
		return err
	}
	if err := ValidateRef(request.Ref); err != nil {
		return err
	}
	return nil
}

// ValidateSourceURL accepts HTTPS Git repositories and HTTPS archives. Local
// paths, SSH URLs, credentials, and obvious private-network destinations are
// rejected before go-getter or git can access them.
func ValidateSourceURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("plugin source URL is required")
	}
	if strings.HasPrefix(raw, "git::") {
		raw = strings.TrimPrefix(raw, "git::")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return nil, errors.New("plugin source must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return nil, errors.New("plugin source URL must not contain credentials")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "localhost" || strings.HasSuffix(hostname, ".localhost") || strings.HasSuffix(hostname, ".local") {
		return nil, errors.New("plugin source host is not allowed")
	}
	if ip := net.ParseIP(hostname); ip != nil && isPrivateAddress(ip) {
		return nil, errors.New("plugin source host is not allowed")
	}
	return parsed, nil
}

// ValidateRef restricts Git refs to characters that cannot become shell
// syntax or an unexpected option. The builder still passes it as an argument,
// so this is a defense-in-depth check rather than shell escaping.
func ValidateRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if len(ref) > 256 || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " ~^:?*[\\\"") {
		return fmt.Errorf("plugin ref %q is not allowed", ref)
	}
	return nil
}

func sourceWithRef(raw, ref string) string {
	prefix := ""
	if strings.HasPrefix(raw, "git::") {
		prefix = "git::"
		raw = strings.TrimPrefix(raw, prefix)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return prefix + raw
	}
	if ref != "" {
		query := parsed.Query()
		query.Set("ref", ref)
		parsed.RawQuery = query.Encode()
	}
	if prefix == "" && !isArchivePath(parsed.Path) {
		prefix = "git::"
	}
	return prefix + parsed.String()
}

func isArchivePath(path string) bool {
	lowerPath := strings.ToLower(path)
	for _, suffix := range []string{".zip", ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz", ".gz"} {
		if strings.HasSuffix(lowerPath, suffix) {
			return true
		}
	}
	return false
}

func newSourceGetter(maxSourceBytes int64) *getter.Client {
	configuredGetters := append([]getter.Getter(nil), getter.Getters...)
	for index, configured := range configuredGetters {
		httpGetter, ok := configured.(*getter.HttpGetter)
		if !ok {
			continue
		}
		configuredHTTPGetter := *httpGetter
		configuredHTTPGetter.Netrc = false
		configuredHTTPGetter.MaxBytes = maxSourceBytes
		configuredHTTPGetter.Client = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(redirectRequest *http.Request, _ []*http.Request) error {
				if _, err := ValidateSourceURL(redirectRequest.URL.String()); err != nil {
					return fmt.Errorf("unsafe plugin source redirect: %w", err)
				}
				return nil
			},
		}
		configuredGetters[index] = &configuredHTTPGetter
	}
	return &getter.Client{Getters: configuredGetters, DisableSymlinks: true}
}

func isPrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func digestSource(root string, maxBytes int64) (string, error) {
	hasher := sha256.New()
	var totalBytes int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source contains a symbolic link: %s", relative)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source contains unsupported file: %s", relative)
		}
		if maxBytes > 0 && totalBytes > maxBytes-info.Size() {
			return fmt.Errorf("source exceeds the %d byte limit", maxBytes)
		}
		totalBytes += info.Size()
		_, _ = io.WriteString(hasher, relative)
		_, _ = io.WriteString(hasher, "\x00")
		_, _ = io.WriteString(hasher, info.Mode().Perm().String())
		_, _ = io.WriteString(hasher, "\x00")
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(hasher, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
