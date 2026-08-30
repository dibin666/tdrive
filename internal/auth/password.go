// Package auth handles local accounts: password hashing, WebUI sessions and
// the HTTP middleware that guards both the REST API and WebDAV.
//
// The accounts here are unrelated to the Telegram login. One tdrive deployment
// is one Telegram account and therefore one drive; these accounts decide who
// may reach it.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. 64 MiB and three passes is the OWASP-recommended
// starting point and takes roughly 50 ms on a modest container, which is fine
// for a login and is exactly why VerifyCache exists for WebDAV.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLength is enforced everywhere a password is set.
const MinPasswordLength = 8

var (
	// ErrBadCredentials is deliberately identical for an unknown user and a
	// wrong password, so the API cannot be used to enumerate accounts.
	ErrBadCredentials = errors.New("auth: incorrect username or password")
	// ErrWeakPassword is returned when a new password is too short.
	ErrWeakPassword = fmt.Errorf("auth: password must be at least %d characters", MinPasswordLength)
	// ErrAccountDisabled is deliberately distinct from ErrBadCredentials: it
	// is only ever returned after the password already checked out, so it
	// tells the caller nothing they did not already prove they knew, and
	// "your account is disabled" is the only message that stops someone
	// retyping a password that was correct all along.
	ErrAccountDisabled = errors.New("auth: this account has been disabled")
	// ErrForbidden is returned when a valid account lacks the permission an
	// action needs.
	ErrForbidden = errors.New("auth: this account is not allowed to do that")
	// ErrBadHash means a stored hash could not be parsed.
	ErrBadHash = errors.New("auth: stored password hash is malformed")
)

// HashPassword produces an encoded argon2id hash in the standard
// "$argon2id$v=19$m=...,t=...,p=...$salt$hash" form, so the parameters travel
// with the hash and can be raised later without invalidating old passwords.
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", ErrWeakPassword
	}

	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	parallelism := uint8(min(runtime.NumCPU(), 4))
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, parallelism, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks a password against an encoded hash in constant time.
func VerifyPassword(encoded, password string) error {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return ErrBadHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return ErrBadHash
	}

	var (
		memory      uint32
		time        uint32
		parallelism uint8
	)
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &parallelism); err != nil {
		return ErrBadHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return ErrBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return ErrBadHash
	}

	got := argon2.IDKey([]byte(password), salt, time, memory, parallelism, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrBadCredentials
	}
	return nil
}
