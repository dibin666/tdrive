package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/dibin/tdrive/internal/config"
)

type sourceBuilder interface {
	Inspect(context.Context, BuilderRequest) (BuilderResponse, error)
	Build(context.Context, BuilderRequest) (BuilderResponse, error)
	Close()
}

// builderClient speaks HTTP to either a Compose sidecar or a lazily started
// standalone builder. The first request pays the process startup cost; normal
// tdrive requests never touch this client.
type builderClient struct {
	settings config.Plugins
	client   *http.Client
	baseURL  string

	mu      sync.Mutex
	process *exec.Cmd
}

func newBuilderClient(settings config.Plugins) (*builderClient, error) {
	client, baseURL, err := newBuilderHTTPClient(settings.BuilderAddress)
	if err != nil {
		return nil, err
	}
	return &builderClient{settings: settings, client: client, baseURL: baseURL}, nil
}

func newBuilderHTTPClient(address string) (*http.Client, string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, "", errors.New("plugin builder address is empty")
	}
	if strings.HasPrefix(address, "unix://") || strings.HasPrefix(address, "/") {
		socketPath := strings.TrimPrefix(address, "unix://")
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
			},
		}
		return &http.Client{Transport: transport}, "http://tdrive-plugin-builder", nil
	}
	if strings.HasPrefix(address, "tcp://") {
		address = "http://" + strings.TrimPrefix(address, "tcp://")
	}
	if !strings.HasPrefix(address, "http://") && !strings.HasPrefix(address, "https://") {
		return nil, "", errors.New("plugin builder address must be a Unix socket or HTTP URL")
	}
	return &http.Client{}, strings.TrimSuffix(address, "/"), nil
}

func (builder *builderClient) Inspect(ctx context.Context, request BuilderRequest) (BuilderResponse, error) {
	var response BuilderResponse
	if err := builder.do(ctx, "/v1/inspect", request, &response); err != nil {
		return BuilderResponse{}, err
	}
	return response, nil
}

func (builder *builderClient) Build(ctx context.Context, request BuilderRequest) (BuilderResponse, error) {
	var response BuilderResponse
	if err := builder.do(ctx, "/v1/build", request, &response); err != nil {
		return BuilderResponse{}, err
	}
	return response, nil
}

func (builder *builderClient) do(ctx context.Context, path string, request, response any) error {
	if err := builder.ensureRunning(ctx); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode builder request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, builder.baseURL+path,
		strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := builder.client.Do(httpRequest)
	if err != nil {
		return fmt.Errorf("connect to plugin builder: %w", err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read plugin builder response: %w", err)
	}
	if httpResponse.StatusCode < http.StatusOK || httpResponse.StatusCode >= http.StatusMultipleChoices {
		var message struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &message) == nil && message.Error != "" {
			return errors.New(message.Error)
		}
		return fmt.Errorf("plugin builder returned HTTP %d", httpResponse.StatusCode)
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("decode plugin builder response: %w", err)
	}
	return nil
}

func (builder *builderClient) ensureRunning(ctx context.Context) error {
	if builder.healthy(ctx) {
		return nil
	}

	builder.mu.Lock()
	defer builder.mu.Unlock()
	if builder.healthy(ctx) {
		return nil
	}
	if builder.settings.BuilderCommand == "" {
		return errors.New("plugin builder is not available")
	}
	if builder.process == nil || builder.process.ProcessState != nil {
		command := exec.Command(builder.settings.BuilderCommand,
			"-socket", builder.settings.BuilderAddress,
			"-output-root", builder.settings.Dir,
			"-max-source-bytes", fmt.Sprintf("%d", builder.settings.SourceMaxBytes),
			"-build-timeout", builder.settings.BuildTimeout.String())
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Start(); err != nil {
			return fmt.Errorf("start plugin builder: %w", err)
		}
		builder.process = command
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if builder.healthy(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("plugin builder did not become ready")
}

func (builder *builderClient) healthy(ctx context.Context) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, builder.baseURL+"/healthz", nil)
	if err != nil {
		return false
	}
	response, err := builder.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func (builder *builderClient) Close() {
	builder.mu.Lock()
	defer builder.mu.Unlock()
	if builder.process != nil && builder.process.Process != nil {
		_ = builder.process.Process.Kill()
		_, _ = builder.process.Process.Wait()
	}
	builder.process = nil
}
