package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

const maxPluginHTTPBody = 8 << 20

// PublicHandler serves plugin-owned routes below /plugins/{id}. Management
// endpoints live below /api/plugins and are mounted by the API package. All
// plugin-owned HTTP routes require a normal tdrive login; a plugin cannot use
// its route declaration to create an unauthenticated entry point.
//
// The check is the browser-aware one: a plugin UI is reached from the WebUI as
// a page rather than as an XHR, so it carries the session cookie instead of the
// in-memory bearer token. Both are accepted.
func (manager *Manager) PublicHandler() http.Handler {
	if manager.auth == nil {
		return http.HandlerFunc(manager.servePublicHTTP)
	}
	return manager.auth.RequireBrowserAuth(http.HandlerFunc(manager.servePublicHTTP))
}

// HTTPMiddleware gives active plugins a last-resort interception point for
// every API request. It is intentionally a single atomic branch when no
// plugin is active, so enabling this capability does not add RPC, JSON, or
// locking work to an ordinary no-plugin deployment.
func (manager *Manager) HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !manager.hooksEnabled.Load() {
			next.ServeHTTP(response, request)
			return
		}
		// Never buffer a large or chunked request. Upload segments can be
		// gigabytes long; their operation metadata is intercepted by the drive
		// hook, while this generic HTTP hook must leave the byte stream intact.
		if request.ContentLength < 0 || request.ContentLength > maxPluginHTTPBody {
			next.ServeHTTP(response, request)
			return
		}

		requestBody, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxPluginHTTPBody))
		if err != nil {
			http.Error(response, "plugin request body is too large", http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(requestBody))
		request.ContentLength = int64(len(requestBody))
		pluginRequest := tdriveplugin.HTTPRequest{
			Method:     request.Method,
			Path:       request.URL.Path,
			RawQuery:   request.URL.RawQuery,
			Headers:    cloneHeaders(request.Header),
			Body:       requestBody,
			UserID:     tdriveplugin.UserIDFromContext(request.Context()),
			RemoteAddr: request.RemoteAddr,
		}
		payload, err := json.Marshal(pluginRequest)
		if err != nil {
			http.Error(response, "could not encode plugin request", http.StatusInternalServerError)
			return
		}
		operation := tdriveplugin.Operation{
			Name:    "http.request",
			UserID:  pluginRequest.UserID,
			Payload: payload,
		}
		// Before dispatches only to the requesting account's plugins, so an
		// unauthenticated request reaches none of them and a signed-in one
		// reaches only its own. A plugin cannot use this hook to watch other
		// people's API traffic.
		result, err := manager.Before(request.Context(), operation)
		if err != nil {
			http.Error(response, "plugin interceptor failed", http.StatusBadGateway)
			return
		}
		if !result.Allowed {
			message := result.Error
			if message == "" {
				message = "request rejected by plugin"
			}
			http.Error(response, message, http.StatusForbidden)
			return
		}
		if len(result.Payload) > 0 && string(result.Payload) != string(payload) {
			var modified tdriveplugin.HTTPRequest
			if err := json.Unmarshal(result.Payload, &modified); err != nil {
				http.Error(response, "plugin returned an invalid request", http.StatusBadGateway)
				return
			}
			if modified.Method != "" {
				request.Method = modified.Method
				request.RequestURI = ""
			}
			if modified.Path != "" {
				request.URL.Path = modified.Path
			}
			if modified.RawQuery != "" {
				request.URL.RawQuery = modified.RawQuery
			}
			if modified.Headers != nil {
				request.Header = pluginHeaders(modified.Headers)
			}
			if modified.Body != nil {
				request.Body = io.NopCloser(bytes.NewReader(modified.Body))
				request.ContentLength = int64(len(modified.Body))
			}
			operation.Payload = result.Payload
		}
		defer manager.After(request.Context(), operation)
		next.ServeHTTP(response, request)
	})
}

func (manager *Manager) servePublicHTTP(response http.ResponseWriter, request *http.Request) {
	pluginID, routePath, ok := splitPluginPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}
	// A plugin route belongs to the account that installed it. Resolving
	// against anyone else's installation would let one account's browser reach
	// another account's child process, which is the whole point of per-account
	// ownership.
	//
	// The answer is 404 rather than 403: PublicHandler has already run the auth
	// check, so an empty user id means the manager was built without an auth
	// service, which only happens in package tests. Keeping "this plugin is not
	// yours" indistinguishable from "no such plugin" is the same choice the API
	// makes for out-of-scope paths.
	userID := tdriveplugin.UserIDFromContext(request.Context())
	if userID == "" {
		http.NotFound(response, request)
		return
	}
	active := manager.getActive(pluginKey{userID: userID, pluginID: pluginID})
	if active == nil || !matchesRoute(active.manifest, routePath, request.Method) {
		http.NotFound(response, request)
		return
	}

	request.Body = http.MaxBytesReader(response, request.Body, maxPluginHTTPBody)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(response, "plugin request body is too large", http.StatusRequestEntityTooLarge)
		return
	}
	pluginRequest := tdriveplugin.HTTPRequest{
		Method:     request.Method,
		Path:       routePath,
		RawQuery:   request.URL.RawQuery,
		Headers:    cloneHeaders(request.Header),
		Body:       body,
		UserID:     userID,
		RemoteAddr: request.RemoteAddr,
	}
	callCtx, cancel := context.WithTimeout(request.Context(), pluginCallTimeout)
	pluginResponse, err := active.client.HandleHTTP(callCtx, pluginRequest)
	cancel()
	if err != nil {
		manager.reportPluginCallError(response, active, request, err)
		return
	}
	for key, values := range pluginResponse.Headers {
		response.Header()[key] = append([]string(nil), values...)
	}
	status := pluginResponse.Status
	if status == 0 {
		status = http.StatusOK
	}
	response.WriteHeader(status)
	_, _ = response.Write(pluginResponse.Body)
}

// reportPluginCallError answers one failed plugin HTTP call and decides whether
// the child process is at fault.
//
// It exists because "the call did not return a response" and "the plugin is
// broken" are not the same thing, and treating them as one restarted a healthy
// child every time a browser walked away from an in-flight request. A plugin
// page that polls cancels request.Context() whenever the operator switches
// tabs, reloads, or closes it; the RPC client returns that cancellation
// verbatim, and reading it as a runtime failure killed the child. For a sync
// plugin that meant every in-flight transfer restarted from zero whenever
// somebody navigated away from its page.
//
// Only a child that has actually exited, or an RPC connection that can no
// longer carry a call, is a failure worth recovering from. A cancellation
// belongs to the caller, and a timeout against a live child is reported to the
// browser without disturbing whatever that child is still doing.
func (manager *Manager) reportPluginCallError(
	response http.ResponseWriter,
	active *activePlugin,
	request *http.Request,
	err error,
) {
	// The client hung up. Nothing is waiting for a response and no plugin
	// misbehaved, so this is not even worth a warning.
	if request.Context().Err() != nil && errors.Is(err, context.Canceled) {
		manager.log.Debug("plugin HTTP request was cancelled by the client",
			zap.String("plugin", active.record.ID), zap.Error(err))
		// 499 is nginx's "client closed request". Nothing reads it here, but it
		// keeps the access log honest about why the handler produced no body.
		response.WriteHeader(clientClosedRequest)
		return
	}

	if errors.Is(err, context.DeadlineExceeded) && !pluginChildExited(active) {
		// The child is alive and simply slower than pluginCallTimeout, which a
		// plugin moving bytes legitimately can be. Report the timeout and record
		// it, but let it keep working.
		manager.log.Warn("plugin HTTP handler timed out",
			zap.String("plugin", active.record.ID), zap.Error(err))
		manager.recordPluginFailure(keyOf(active.record), err)
		http.Error(response, "plugin handler timed out", http.StatusGatewayTimeout)
		return
	}

	manager.log.Warn("plugin HTTP handler failed", zap.String("plugin", active.record.ID), zap.Error(err))
	manager.handlePluginFailure(keyOf(active.record), err)
	http.Error(response, "plugin handler failed", http.StatusBadGateway)
}

// clientClosedRequest is nginx's non-standard 499, used here for the same
// reason: the request ended because the client went away, and no other status
// says that without blaming the server or the plugin.
const clientClosedRequest = 499

// pluginChildExited reports whether the child process is gone. A runtime built
// by a test has no process, and is treated as alive so the caller falls back to
// classifying by error alone.
func pluginChildExited(active *activePlugin) bool {
	return active != nil && active.process != nil && active.process.Exited()
}

func splitPluginPath(path string) (string, string, bool) {
	trimmed := strings.TrimPrefix(path, "/plugins/")
	if trimmed == path || trimmed == "" {
		return "", "", false
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if parts[0] == "" {
		return "", "", false
	}
	routePath := "/"
	if len(parts) == 2 && parts[1] != "" {
		routePath += parts[1]
	}
	return parts[0], routePath, true
}

func matchesRoute(manifest tdriveplugin.Manifest, path, method string) bool {
	for _, route := range manifest.Routes {
		methodMatches := len(route.Methods) == 0
		for _, allowed := range route.Methods {
			if strings.EqualFold(allowed, method) {
				methodMatches = true
				break
			}
		}
		if !methodMatches {
			continue
		}
		if route.Path == path {
			return true
		}
		if strings.HasSuffix(route.Path, "/*") {
			prefix := strings.TrimSuffix(route.Path, "*")
			if strings.HasPrefix(path, prefix) {
				return true
			}
		}
	}
	return false
}

func cloneHeaders(headers http.Header) map[string][]string {
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func pluginHeaders(headers map[string][]string) http.Header {
	converted := make(http.Header, len(headers))
	for key, values := range headers {
		converted[key] = append([]string(nil), values...)
	}
	return converted
}
