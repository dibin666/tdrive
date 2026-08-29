package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
)

func newTestService(t *testing.T) (*Service, *database.DB) {
	t.Helper()
	ctx := context.Background()

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	cfg := &config.Config{}
	cfg.Auth.AccessTTL = 15 * time.Minute
	cfg.Auth.RefreshTTL = 24 * time.Hour

	svc, err := New(ctx, cfg, db)
	if err != nil {
		t.Fatalf("new auth service: %v", err)
	}
	return svc, db
}

func TestPasswordHashing(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("unexpected hash format: %q", hash)
	}
	if err := VerifyPassword(hash, "correct horse battery"); err != nil {
		t.Errorf("the correct password was rejected: %v", err)
	}
	if err := VerifyPassword(hash, "wrong"); err == nil {
		t.Error("a wrong password was accepted")
	}

	// Two hashes of the same password must differ, or the salt is not doing
	// its job and identical passwords would be visible in the database.
	other, _ := HashPassword("correct horse battery")
	if other == hash {
		t.Error("hashing is not salted")
	}

	if _, err := HashPassword("short"); err == nil {
		t.Error("a password under the minimum length was accepted")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	for _, bad := range []string{
		"", "notahash", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=99$m=1,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=19$garbage$AAAA$AAAA",
		"$argon2id$v=19$m=1,t=1,p=1$!!!!$AAAA",
	} {
		if err := VerifyPassword(bad, "anything"); err == nil {
			t.Errorf("malformed hash %q was accepted", bad)
		}
	}
}

func TestLoginAndRefreshRotation(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "alice", "hunter2hunter2", database.RoleAdmin); err != nil {
		t.Fatalf("create user: %v", err)
	}

	tokens, user, err := svc.Login(ctx, "alice", "hunter2hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if user.Username != "alice" {
		t.Errorf("logged in as %q", user.Username)
	}

	claims, err := svc.Parse(tokens.Access)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	if claims.Subject != user.ID || claims.Role != database.RoleAdmin {
		t.Errorf("claims = %+v", claims)
	}

	// Refreshing must rotate: the old refresh token has to stop working, so a
	// stolen one is usable at most once.
	next, _, err := svc.Refresh(ctx, tokens.Refresh)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if next.Refresh == tokens.Refresh {
		t.Error("the refresh token was not rotated")
	}
	if _, _, err := svc.Refresh(ctx, tokens.Refresh); err == nil {
		t.Error("the old refresh token still works after rotation")
	}
	if _, _, err := svc.Refresh(ctx, next.Refresh); err != nil {
		t.Errorf("the rotated token does not work: %v", err)
	}
}

func TestLoginFailsClosed(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, "bob", "hunter2hunter2", database.RoleUser); err != nil {
		t.Fatalf("create user: %v", err)
	}

	for _, tc := range []struct{ user, pass string }{
		{"bob", "wrong"},
		{"nobody", "hunter2hunter2"},
		{"", ""},
	} {
		if _, _, err := svc.Login(ctx, tc.user, tc.pass); err == nil {
			t.Errorf("login as %q/%q succeeded", tc.user, tc.pass)
		}
	}
}

func TestChangePasswordEndsSessions(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "carol", "hunter2hunter2", database.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	tokens, _, err := svc.Login(ctx, "carol", "hunter2hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := svc.ChangePassword(ctx, user.ID, "newpassword123"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, _, err := svc.Refresh(ctx, tokens.Refresh); err == nil {
		t.Error("an existing session survived a password change")
	}
	if _, _, err := svc.Login(ctx, "carol", "hunter2hunter2"); err == nil {
		t.Error("the old password still works")
	}
	if _, _, err := svc.Login(ctx, "carol", "newpassword123"); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// WebDAV clients resend credentials on every request, so a verified pair is
// cached. The cache must not outlive a password change.
func TestBasicAuthCacheRespectsPasswordChanges(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "dave", "hunter2hunter2", database.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	for range 3 {
		if _, err := svc.VerifyBasic(ctx, "dave", "hunter2hunter2"); err != nil {
			t.Fatalf("verify: %v", err)
		}
	}
	if _, err := svc.VerifyBasic(ctx, "dave", "wrong"); err == nil {
		t.Error("a wrong password passed Basic auth")
	}

	if err := svc.ChangePassword(ctx, user.ID, "adifferentone"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.VerifyBasic(ctx, "dave", "hunter2hunter2"); err == nil {
		t.Error("the cache kept accepting the old password after it changed")
	}
	if _, err := svc.VerifyBasic(ctx, "dave", "adifferentone"); err != nil {
		t.Errorf("the new password was rejected: %v", err)
	}
}

// A media token authorises one file for a bounded time and nothing else,
// because it travels in a URL where a session token must never go.
func TestMediaTokenScope(t *testing.T) {
	svc, _ := newTestService(t)

	token := svc.SignMediaToken("file-a")
	if !svc.VerifyMediaToken("file-a", token) {
		t.Error("a freshly signed token was rejected")
	}
	if svc.VerifyMediaToken("file-b", token) {
		t.Error("a token for one file authorised another")
	}

	for _, bad := range []string{"", "garbage", "999.abc", token + "x", strings.Replace(token, ".", "", 1)} {
		if svc.VerifyMediaToken("file-a", bad) {
			t.Errorf("forged token %q was accepted", bad)
		}
	}

	// An expired token must be refused even though its signature is valid.
	expired := "1.oldsignature"
	if svc.VerifyMediaToken("file-a", expired) {
		t.Error("an expired token was accepted")
	}
}

func TestMiddlewareGuardsRoutes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, "erin", "hunter2hunter2", database.RoleUser); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := svc.CreateUser(ctx, "root", "hunter2hunter2", database.RoleAdmin); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := FromContext(r.Context())
		_ = json.NewEncoder(w).Encode(map[string]string{"user": u.Username})
	})
	protected := svc.RequireAuth(ok)
	adminOnly := svc.RequireAuth(svc.RequireAdmin(ok))
	dav := svc.RequireBasic(ok)

	call := func(h http.Handler, setup func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/", nil)
		if setup != nil {
			setup(req)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// No credentials.
	if rec := call(protected, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request got %d, want 401", rec.Code)
	}
	if rec := call(dav, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated WebDAV request got %d, want 401", rec.Code)
	} else if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("WebDAV challenge is %q, want a Basic challenge", got)
	}

	// The API must not send a Basic challenge, or browsers pop their native
	// credential dialog over the login form.
	if rec := call(protected, nil); rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("the API sent a Basic challenge")
	}

	// Bearer token.
	tokens, _, err := svc.Login(ctx, "erin", "hunter2hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if rec := call(protected, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tokens.Access)
	}); rec.Code != http.StatusOK {
		t.Errorf("bearer request got %d, want 200", rec.Code)
	}

	// A non-admin must not reach admin routes.
	if rec := call(adminOnly, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+tokens.Access)
	}); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin on an admin route got %d, want 403", rec.Code)
	}

	adminTokens, _, err := svc.Login(ctx, "root", "hunter2hunter2")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	if rec := call(adminOnly, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+adminTokens.Access)
	}); rec.Code != http.StatusOK {
		t.Errorf("admin on an admin route got %d, want 200", rec.Code)
	}

	// Basic works on both, since tools should be able to use one credential.
	if rec := call(dav, func(r *http.Request) {
		r.SetBasicAuth("erin", "hunter2hunter2")
	}); rec.Code != http.StatusOK {
		t.Errorf("Basic on WebDAV got %d, want 200", rec.Code)
	}
	if rec := call(protected, func(r *http.Request) {
		r.SetBasicAuth("erin", "hunter2hunter2")
	}); rec.Code != http.StatusOK {
		t.Errorf("Basic on the API got %d, want 200", rec.Code)
	}

	// Garbage credentials.
	for _, header := range []string{"Bearer nonsense", "Bearer ", "Basic bm9wZQ==", "Nonsense abc"} {
		if rec := call(protected, func(r *http.Request) {
			r.Header.Set("Authorization", header)
		}); rec.Code != http.StatusUnauthorized {
			t.Errorf("header %q got %d, want 401", header, rec.Code)
		}
	}
}

// A deleted account's outstanding access token must stop working immediately
// rather than at expiry.
func TestDeletedUserLosesAccessImmediately(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	user, err := svc.CreateUser(ctx, "frank", "hunter2hunter2", database.RoleUser)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	tokens, _, err := svc.Login(ctx, "frank", "hunter2hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	handler := svc.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.Access)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("before deletion: got %d", rec.Code)
	}

	if err := db.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.Access)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a deleted user's token still works: got %d", rec.Code)
	}
}

func TestBootstrapSeedsOnlyOnce(t *testing.T) {
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "boot.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	cfg := &config.Config{}
	cfg.Auth.AccessTTL = time.Minute
	cfg.Auth.RefreshTTL = time.Hour
	cfg.Auth.BootstrapUser = "admin"
	cfg.Auth.BootstrapPassword = "hunter2hunter2"

	svc, err := New(ctx, cfg, db)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := svc.Bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// Changing the password and restarting must not reset it.
	user, err := db.UserByName(ctx, "admin")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if err := svc.ChangePassword(ctx, user.ID, "somethingelse"); err != nil {
		t.Fatalf("change: %v", err)
	}
	if err := svc.Bootstrap(ctx); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if _, _, err := svc.Login(ctx, "admin", "somethingelse"); err != nil {
		t.Errorf("bootstrap reset a password that had been changed: %v", err)
	}
}
