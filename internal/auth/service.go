package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/dibin/tdrive/internal/config"
	"github.com/dibin/tdrive/internal/database"
)

// Service issues and validates credentials for both entry points: short-lived
// JWTs for the WebUI, HTTP Basic for WebDAV.
type Service struct {
	db     *database.DB
	cfg    *config.Config
	secret []byte

	verified *verifyCache
}

// Claims is the WebUI access token payload.
type Claims struct {
	jwt.RegisteredClaims
	Username string        `json:"username"`
	Role     database.Role `json:"role"`
}

// New loads or creates the token signing secret.
//
// Generating and persisting a secret means a deployment that sets nothing but a
// data volume still keeps its sessions across restarts, instead of logging
// everyone out whenever the container is recreated.
func New(ctx context.Context, cfg *config.Config, db *database.DB) (*Service, error) {
	secret := cfg.Auth.JWTSecret
	if len(secret) == 0 {
		stored, err := db.Setting(ctx, database.SettingJWTSecret)
		switch {
		case err == nil:
			secret, err = base64.StdEncoding.DecodeString(stored)
			if err != nil {
				return nil, fmt.Errorf("auth: stored signing secret is corrupt: %w", err)
			}
		case errors.Is(err, database.ErrNotFound):
			secret = make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return nil, fmt.Errorf("auth: generate signing secret: %w", err)
			}
			if err := db.SetSetting(ctx, database.SettingJWTSecret,
				base64.StdEncoding.EncodeToString(secret)); err != nil {
				return nil, err
			}
		default:
			return nil, err
		}
	}

	return &Service{
		db:       db,
		cfg:      cfg,
		secret:   secret,
		verified: newVerifyCache(5 * time.Minute),
	}, nil
}

// Bootstrap seeds the first administrator from the environment. It is a no-op
// once any account exists, so restarting with the variables still set does not
// reset a password that has since been changed.
func (s *Service) Bootstrap(ctx context.Context) error {
	if s.cfg.Auth.BootstrapUser == "" {
		return nil
	}
	n, err := s.db.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = s.CreateUser(ctx, s.cfg.Auth.BootstrapUser, s.cfg.Auth.BootstrapPassword, database.RoleAdmin)
	return err
}

// NeedsSetup reports whether the WebUI should show the first-run wizard.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := s.db.CountUsers(ctx)
	return n == 0, err
}

func (s *Service) CreateUser(ctx context.Context, username, password string, role database.Role) (database.User, error) {
	if username == "" {
		return database.User{}, errors.New("auth: username is required")
	}
	hash, err := HashPassword(password)
	if err != nil {
		return database.User{}, err
	}
	return s.db.CreateUser(ctx, username, hash, role)
}

// Login verifies credentials and mints a token pair.
func (s *Service) Login(ctx context.Context, username, password string) (Tokens, database.User, error) {
	user, err := s.db.UserByName(ctx, username)
	if err != nil {
		if errors.Is(err, database.ErrNotFound) {
			// Hash anyway so that a missing account and a wrong password
			// take the same amount of time.
			_ = VerifyPassword("$argon2id$v=19$m=65536,t=3,p=2$"+
				"AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", password)
			return Tokens{}, database.User{}, ErrBadCredentials
		}
		return Tokens{}, database.User{}, err
	}

	if err := VerifyPassword(user.PasswordHash, password); err != nil {
		return Tokens{}, database.User{}, ErrBadCredentials
	}

	tokens, err := s.issue(ctx, user)
	return tokens, user, err
}

// Tokens is what a successful login hands back. The refresh token is meant for
// an HttpOnly cookie; the access token goes in the Authorization header.
type Tokens struct {
	Access       string    `json:"accessToken"`
	AccessExpiry time.Time `json:"accessExpiry"`
	Refresh      string    `json:"-"`
	RefreshExp   time.Time `json:"-"`
}

func (s *Service) issue(ctx context.Context, user database.User) (Tokens, error) {
	now := time.Now()
	accessExp := now.Add(s.cfg.Auth.AccessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(accessExp),
			Issuer:    "tdrive",
		},
		Username: user.Username,
		Role:     user.Role,
	}
	access, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.secret)
	if err != nil {
		return Tokens{}, fmt.Errorf("auth: sign access token: %w", err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Tokens{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}
	refresh := base64.RawURLEncoding.EncodeToString(raw)
	refreshExp := now.Add(s.cfg.Auth.RefreshTTL)

	// Only the hash is stored, so a database leak cannot be replayed into
	// live sessions.
	if _, err := s.db.StoreRefreshToken(ctx, user.ID, hashToken(refresh), refreshExp); err != nil {
		return Tokens{}, err
	}

	return Tokens{
		Access:       access,
		AccessExpiry: accessExp,
		Refresh:      refresh,
		RefreshExp:   refreshExp,
	}, nil
}

// Refresh exchanges a refresh token for a new pair, rotating the refresh token
// so a stolen one is usable at most once.
func (s *Service) Refresh(ctx context.Context, refresh string) (Tokens, database.User, error) {
	userID, tokenID, err := s.db.LookupRefreshToken(ctx, hashToken(refresh))
	if err != nil {
		return Tokens{}, database.User{}, ErrBadCredentials
	}
	user, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return Tokens{}, database.User{}, ErrBadCredentials
	}
	if err := s.db.RevokeRefreshToken(ctx, tokenID); err != nil {
		return Tokens{}, database.User{}, err
	}

	tokens, err := s.issue(ctx, user)
	return tokens, user, err
}

// Logout revokes one session.
func (s *Service) Logout(ctx context.Context, refresh string) error {
	_, tokenID, err := s.db.LookupRefreshToken(ctx, hashToken(refresh))
	if err != nil {
		return nil // already gone; nothing to do
	}
	return s.db.RevokeRefreshToken(ctx, tokenID)
}

// Parse validates an access token.
func (s *Service) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return s.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer("tdrive"))
	if err != nil || !parsed.Valid {
		return nil, ErrBadCredentials
	}
	return claims, nil
}

// VerifyBasic authenticates a WebDAV request.
//
// WebDAV clients resend credentials on every single request, and a PROPFIND
// storm can be hundreds of them. Running argon2id each time would add tens of
// milliseconds and 64 MiB of allocation per request, so a successful check is
// remembered briefly. The cache stores a salted hash of the password, never the
// password, and expires quickly enough that a password change takes effect
// without a restart.
func (s *Service) VerifyBasic(ctx context.Context, username, password string) (database.User, error) {
	if cached, ok := s.verified.get(username, password); ok {
		return cached, nil
	}

	user, err := s.db.UserByName(ctx, username)
	if err != nil {
		return database.User{}, ErrBadCredentials
	}
	if err := VerifyPassword(user.PasswordHash, password); err != nil {
		return database.User{}, ErrBadCredentials
	}

	s.verified.put(username, password, user)
	return user, nil
}

// InvalidateUser drops cached Basic auth for an account, called when its
// password or role changes.
func (s *Service) InvalidateUser(username string) { s.verified.forget(username) }

// ChangePassword updates a password and ends every existing session for that
// account, because the old sessions were established under the old secret.
func (s *Service) ChangePassword(ctx context.Context, userID, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.db.SetUserPassword(ctx, userID, hash); err != nil {
		return err
	}
	if user, err := s.db.UserByID(ctx, userID); err == nil {
		s.verified.forget(user.Username)
	}
	return s.db.RevokeUserTokens(ctx, userID)
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// MediaTokenTTL bounds how long a media link stays usable.
const MediaTokenTTL = 6 * time.Hour

// SignMediaToken mints a token that authorises reading one file's bytes.
//
// A <video> element and a download link cannot carry an Authorization header,
// so something has to travel in the URL. Putting the session token there would
// leak a full account credential into browser history, proxy logs and any
// Referer that escapes; this instead authorises exactly one file for a bounded
// time and grants nothing else.
func (s *Service) SignMediaToken(fileID string) string {
	expiry := time.Now().Add(MediaTokenTTL).Unix()
	payload := fmt.Sprintf("%s.%d", fileID, expiry)

	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("tdrive-media\x00"))
	mac.Write([]byte(payload))

	return fmt.Sprintf("%d.%s", expiry, base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

// VerifyMediaToken checks a token against the file it claims to authorise.
func (s *Service) VerifyMediaToken(fileID, token string) bool {
	expiryStr, sig, found := strings.Cut(token, ".")
	if !found {
		return false
	}
	expiry, err := strconv.ParseInt(expiryStr, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}

	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte("tdrive-media\x00"))
	mac.Write([]byte(fmt.Sprintf("%s.%d", fileID, expiry)))

	want, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	return hmac.Equal(mac.Sum(nil), want)
}

// verifyCache remembers recently verified Basic auth credentials.
type verifyCache struct {
	ttl  time.Duration
	salt []byte

	mu      sync.RWMutex
	entries map[string]verifyEntry
}

type verifyEntry struct {
	user    database.User
	expires time.Time
}

func newVerifyCache(ttl time.Duration) *verifyCache {
	// A per-process salt means the cache keys are meaningless outside this
	// run and cannot be precomputed against.
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	return &verifyCache{ttl: ttl, salt: salt, entries: make(map[string]verifyEntry)}
}

func (c *verifyCache) key(username, password string) string {
	h := sha256.New()
	h.Write(c.salt)
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *verifyCache) get(username, password string) (database.User, bool) {
	k := c.key(username, password)
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[k]
	if !ok || time.Now().After(e.expires) {
		return database.User{}, false
	}
	return e.user, true
}

func (c *verifyCache) put(username, password string, user database.User) {
	k := c.key(username, password)
	c.mu.Lock()
	defer c.mu.Unlock()

	// Bounded so a client hammering with wrong-but-varying passwords cannot
	// grow it without limit. Successful credentials are few, so a small cap
	// is plenty and a full reset is cheap.
	if len(c.entries) > 1024 {
		c.entries = make(map[string]verifyEntry)
	}
	c.entries[k] = verifyEntry{user: user, expires: time.Now().Add(c.ttl)}
}

func (c *verifyCache) forget(username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if e.user.Username == username {
			delete(c.entries, k)
		}
	}
}
