package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

const sha256HexLength = sha256.Size * 2

// Store downloads and filters the configured index. An empty store URL is a
// valid disabled-store configuration and returns an empty list without any
// network request.
func (manager *Manager) Store(ctx context.Context, query string) (StoreIndex, error) {
	if strings.TrimSpace(manager.cfg.Plugins.StoreURL) == "" {
		return StoreIndex{Plugins: []StorePlugin{}}, nil
	}
	if _, err := ValidateSourceURL(manager.cfg.Plugins.StoreURL); err != nil {
		return StoreIndex{}, fmt.Errorf("invalid plugin store URL: %w", err)
	}
	parsed, err := url.Parse(manager.cfg.Plugins.StoreURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return StoreIndex{}, errors.New("plugin store URL must be an absolute HTTPS URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return StoreIndex{}, err
	}
	storeClient := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(redirectRequest *http.Request, _ []*http.Request) error {
			if _, err := ValidateSourceURL(redirectRequest.URL.String()); err != nil {
				return fmt.Errorf("plugin store redirect is unsafe: %w", err)
			}
			return nil
		},
	}
	response, err := storeClient.Do(request)
	if err != nil {
		return StoreIndex{}, fmt.Errorf("fetch plugin store: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return StoreIndex{}, fmt.Errorf("plugin store returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return StoreIndex{}, fmt.Errorf("read plugin store: %w", err)
	}
	var index StoreIndex
	if err := json.Unmarshal(body, &index); err != nil {
		return StoreIndex{}, fmt.Errorf("decode plugin store: %w", err)
	}
	if index.Plugins == nil {
		index.Plugins = []StorePlugin{}
	}
	for index, item := range index.Plugins {
		if err := item.Validate(); err != nil {
			return StoreIndex{}, fmt.Errorf("plugin store item %d: %w", index, err)
		}
	}
	if query = strings.ToLower(strings.TrimSpace(query)); query != "" {
		filtered := index.Plugins[:0]
		for _, item := range index.Plugins {
			if storeItemContains(item, query) {
				filtered = append(filtered, item)
			}
		}
		index.Plugins = filtered
	}
	return index, nil
}

// Validate checks an index item before it reaches the install flow. The
// installer still fetches and validates the source manifest itself; store
// metadata is never trusted as a replacement for that check.
func (item StorePlugin) Validate() error {
	manifest := tdriveplugin.Manifest{
		ID:               item.ID,
		Name:             item.Name,
		Version:          item.Version,
		SDKVersion:       "store",
		APIVersion:       tdriveplugin.APIVersion,
		Author:           item.Author,
		License:          item.License,
		RepositoryURL:    item.RepositoryURL,
		DocumentationURL: item.Documentation,
		Entrypoint:       "./store",
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if item.SourceURL != "" {
		if _, err := ValidateSourceURL(item.SourceURL); err != nil {
			return err
		}
	}
	if err := ValidateRef(item.Ref); err != nil {
		return err
	}
	if len(item.SourceDigest) != sha256HexLength {
		return errors.New("sourceDigest must be a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(item.SourceDigest); err != nil {
		return errors.New("sourceDigest must be a SHA-256 hex digest")
	}
	if strings.TrimSpace(item.License) == "" {
		return errors.New("plugin license is required")
	}
	return nil
}

func storeItemContains(item StorePlugin, query string) bool {
	if strings.Contains(strings.ToLower(item.ID), query) ||
		strings.Contains(strings.ToLower(item.Name), query) ||
		strings.Contains(strings.ToLower(item.Description), query) ||
		strings.Contains(strings.ToLower(item.Author), query) {
		return true
	}
	for _, tag := range item.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}

// StoreStatus adds local installation state to a store result when the UI
// wants to show an installed marker without making one request per item.
type StoreStatus struct {
	StorePlugin
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion,omitempty"`
}

func (manager *Manager) StoreWithStatus(ctx context.Context, query string) ([]StoreStatus, error) {
	index, err := manager.Store(ctx, query)
	if err != nil {
		return nil, err
	}
	installed, err := manager.db.ListPlugins(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]database.PluginRecord, len(installed))
	for _, record := range installed {
		byID[record.ID] = record
	}
	result := make([]StoreStatus, 0, len(index.Plugins))
	for _, item := range index.Plugins {
		status := StoreStatus{StorePlugin: item}
		if record, ok := byID[item.ID]; ok {
			status.Installed = true
			status.InstalledVersion = record.Version
		}
		result = append(result, status)
	}
	return result, nil
}
