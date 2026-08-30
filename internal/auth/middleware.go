package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/dibin/tdrive/internal/database"
	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

type ctxKey int

const userKey ctxKey = iota

// RefreshCookie is the name of the HttpOnly cookie holding the refresh token.
const RefreshCookie = "tdrive_refresh"

// FromContext returns the authenticated account, if any.
func FromContext(ctx context.Context) (database.User, bool) {
	u, ok := ctx.Value(userKey).(database.User)
	return u, ok
}

// WithUser attaches an account to a context.
func WithUser(ctx context.Context, u database.User) context.Context {
	return context.WithValue(ctx, userKey, u)
}

// RequireAuth guards the REST API. It accepts a bearer token, and falls back to
// Basic so that scripts and tools can reach the API with the same credentials
// they use for WebDAV.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.authenticate(r)
		if !ok {
			writeUnauthorized(w, false)
			return
		}
		requestContext := WithUser(r.Context(), user)
		requestContext = tdriveplugin.WithUserID(requestContext, user.ID)
		next.ServeHTTP(w, r.WithContext(requestContext))
	})
}

// RequireBrowserAuth is the session-aware variant used by pages opened through
// a normal browser navigation. Such a navigation cannot attach the in-memory
// bearer token, but it does send the HttpOnly refresh cookie. The cookie is
// checked without rotating it, so opening a plugin UI does not unexpectedly
// invalidate other API requests using the current access token.
func (s *Service) RequireBrowserAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.authenticate(r)
		if !ok {
			cookie, cookieErr := r.Cookie(RefreshCookie)
			if cookieErr == nil {
				user, cookieErr = s.UserForRefresh(r.Context(), cookie.Value)
				ok = cookieErr == nil
			}
		}
		if !ok {
			writeUnauthorized(w, false)
			return
		}
		requestContext := WithUser(r.Context(), user)
		requestContext = tdriveplugin.WithUserID(requestContext, user.ID)
		next.ServeHTTP(w, r.WithContext(requestContext))
	})
}

// UserForRefresh resolves the account behind a refresh cookie without issuing
// a new token pair. It is intentionally narrow so browser page navigation can
// reuse the same session check without exposing refresh-token internals.
func (s *Service) UserForRefresh(ctx context.Context, refresh string) (database.User, error) {
	userID, _, err := s.db.LookupRefreshToken(ctx, HashRefreshToken(refresh))
	if err != nil {
		return database.User{}, ErrBadCredentials
	}
	user, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return database.User{}, ErrBadCredentials
	}
	if !user.Enabled {
		return database.User{}, ErrAccountDisabled
	}
	return user, nil
}

// RequireAdmin guards the endpoints that reconfigure the drive itself.
func (s *Service) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := FromContext(r.Context())
		if !ok {
			writeUnauthorized(w, false)
			return
		}
		if user.Role != database.RoleAdmin {
			writeJSONError(w, http.StatusForbidden, "this action requires an administrator account")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequirePerm guards an endpoint on a single fine-grained permission. It runs
// after RequireAuth, which is what put the account in the context.
func (s *Service) RequirePerm(perm database.Perm) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := FromContext(r.Context())
			if !ok {
				writeUnauthorized(w, false)
				return
			}
			if !user.Can(perm) {
				writeJSONError(w, http.StatusForbidden, ErrForbidden.Error())
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireBasic guards WebDAV.
//
// The challenge has to be Basic: WebDAV clients built into Windows Explorer,
// macOS Finder and rclone all expect it, and none of them will negotiate a
// bearer token.
func (s *Service) RequireBasic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok {
			writeUnauthorized(w, true)
			return
		}
		user, err := s.VerifyBasic(r.Context(), username, password)
		if err != nil {
			writeUnauthorized(w, true)
			return
		}
		// WebDAV is a separate door into the same drive, so it gets its own
		// permission. Answering 403 rather than re-challenging matters: a
		// client that is told to authenticate again will loop asking the user
		// for a password that was never the problem.
		if !user.Can(database.PermWebDAV) {
			writeJSONError(w, http.StatusForbidden, "this account is not allowed to use WebDAV")
			return
		}
		requestContext := WithUser(r.Context(), user)
		requestContext = tdriveplugin.WithUserID(requestContext, user.ID)
		next.ServeHTTP(w, r.WithContext(requestContext))
	})
}

// authenticate resolves a request's credentials from any supported scheme.
func (s *Service) authenticate(r *http.Request) (database.User, bool) {
	header := r.Header.Get("Authorization")

	if after, found := strings.CutPrefix(header, "Bearer "); found {
		claims, err := s.Parse(strings.TrimSpace(after))
		if err != nil {
			return database.User{}, false
		}
		// The token is self-contained, but the account is re-read so that a
		// deleted or disabled user's outstanding token stops working
		// immediately rather than at expiry.
		user, err := s.db.UserByID(r.Context(), claims.Subject)
		if err != nil || !user.Enabled {
			return database.User{}, false
		}
		return user, true
	}

	if username, password, ok := r.BasicAuth(); ok {
		user, err := s.VerifyBasic(r.Context(), username, password)
		if err != nil {
			return database.User{}, false
		}
		return user, true
	}

	return database.User{}, false
}

// writeUnauthorized answers a failed check. The Basic challenge is only sent on
// the WebDAV routes: adding it to the API would make a browser pop up its
// native credential dialog over the WebUI's own login form.
func writeUnauthorized(w http.ResponseWriter, challenge bool) {
	if challenge {
		w.Header().Set("WWW-Authenticate", `Basic realm="tdrive", charset="UTF-8"`)
	}
	writeJSONError(w, http.StatusUnauthorized, "authentication required")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// SetRefreshCookie stores a refresh token in a cookie the browser cannot read
// from script.
func SetRefreshCookie(w http.ResponseWriter, tokens Tokens, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookie,
		Value:    tokens.Refresh,
		Path:     "/",
		Expires:  tokens.RefreshExp,
		HttpOnly: true,
		Secure:   secure,
		// Lax rather than Strict so that following a link into the WebUI
		// keeps the session, while cross-site POSTs still cannot use it.
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearRefreshCookie removes the session cookie on logout.
func ClearRefreshCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     RefreshCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
