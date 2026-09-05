package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Store downloads and filters the configured index. An empty store URL is a
// valid disabled-store configuration and returns an empty list without any
// network request.
func (manager *Manager) Store(ctx context.Context, query string) (StoreIndex, error) {
	if strings.TrimSpace(manager.cfg.Plugins.StoreURL) == "" {
		return StoreIndex{Plugins: []StorePlugin{}}, nil
	}
	parsed, err := ValidateDownloadURL(manager.cfg.Plugins.StoreURL)
	if err != nil {
		return StoreIndex{}, fmt.Errorf("invalid plugin store URL: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return StoreIndex{}, err
	}
	response, err := httpsClient(ValidateDownloadURL, 20*time.Second).Do(request)
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
// installer still fetches and validates the manifest itself; store metadata is
// never trusted as a replacement for that check. What the index does
// contribute is manifestDigest, which pins the manifest the curator reviewed.
func (item StorePlugin) Validate() error {
	// A store entry carries only discovery metadata, so the shared manifest
	// rules are reused for the descriptive fields. The artifact table and SDK
	// version live in the real manifest that the installer downloads.
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
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if _, err := ValidateDownloadURL(item.ManifestURL); err != nil {
		return err
	}
	if !sha256HexPattern.MatchString(item.ManifestDigest) {
		return errors.New("manifestDigest must be 64 lowercase hexadecimal characters")
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

// StoreWithStatus marks the store index against one account's installations.
// The index itself is a public document and stays deployment-wide; only the
// "installed" marker is personal, because somebody else having installed a
// plugin says nothing about whether this account has it.
func (manager *Manager) StoreWithStatus(ctx context.Context, userID, query string) ([]StoreStatus, error) {
	index, err := manager.Store(ctx, query)
	if err != nil {
		return nil, err
	}
	installed, err := manager.db.ListPlugins(ctx, userID)
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
