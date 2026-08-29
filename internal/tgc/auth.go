package tgc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

// The login flow is split across three HTTP requests because that is how the
// user experiences it: type a phone number, wait for a code, then possibly a
// 2FA password. Telegram's phone_code_hash ties them together and is held in
// Manager between calls.

// CodeDelivery tells the WebUI where the user should look for the code.
type CodeDelivery string

const (
	DeliveryApp       CodeDelivery = "app"
	DeliverySMS       CodeDelivery = "sms"
	DeliveryCall      CodeDelivery = "call"
	DeliveryFlashCall CodeDelivery = "flash_call"
	DeliveryMissed    CodeDelivery = "missed_call"
	DeliveryEmail     CodeDelivery = "email"
	DeliveryFragment  CodeDelivery = "fragment"
	DeliveryOther     CodeDelivery = "other"
)

// SendCodeResult is what the UI needs to render the code entry step.
type SendCodeResult struct {
	Delivery   CodeDelivery `json:"delivery"`
	CodeLength int          `json:"codeLength,omitempty"`
	// AlreadyAuthorized is true when the stored session turned out to be
	// valid, in which case no code was sent and the UI can skip ahead.
	AlreadyAuthorized bool `json:"alreadyAuthorized"`
}

// SendCode starts a login. The client must already be connected; Start does
// that as soon as app credentials exist.
func (m *Manager) SendCode(ctx context.Context, phone string) (SendCodeResult, error) {
	client, err := m.Raw()
	if err != nil {
		return SendCodeResult{}, err
	}

	phone = normalisePhone(phone)
	if phone == "" {
		return SendCodeResult{}, errors.New("phone number is required")
	}

	if status, err := client.Auth().Status(ctx); err == nil && status.Authorized {
		m.refreshAuth(ctx)
		return SendCodeResult{AlreadyAuthorized: true}, nil
	}

	sent, err := client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return SendCodeResult{}, fmt.Errorf("send login code: %w", friendly(err))
	}

	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		// AuthSentCodeSuccess means Telegram signed us in without a code,
		// which happens when another authorized session approves the login.
		m.refreshAuth(ctx)
		return SendCodeResult{AlreadyAuthorized: true}, nil
	}

	m.mu.Lock()
	m.loginPhone = phone
	m.loginCodeHash = code.PhoneCodeHash
	m.awaitingPass = false
	m.mu.Unlock()

	delivery, length := describeCode(code.Type)
	m.log.Info("login code sent", zap.String("delivery", string(delivery)))
	return SendCodeResult{Delivery: delivery, CodeLength: length}, nil
}

// SignInResult reports whether a second factor is still needed.
type SignInResult struct {
	NeedsPassword bool `json:"needsPassword"`
	// PasswordHint is Telegram's own hint for the 2FA password, if set.
	PasswordHint string `json:"passwordHint,omitempty"`
}

// SignIn submits the code from SendCode.
func (m *Manager) SignIn(ctx context.Context, code string) (SignInResult, error) {
	client, err := m.Raw()
	if err != nil {
		return SignInResult{}, err
	}

	m.mu.RLock()
	phone, hash := m.loginPhone, m.loginCodeHash
	m.mu.RUnlock()
	if phone == "" || hash == "" {
		return SignInResult{}, errors.New("no login in progress; request a code first")
	}

	_, err = client.Auth().SignIn(ctx, phone, strings.TrimSpace(code), hash)
	switch {
	case errors.Is(err, auth.ErrPasswordAuthNeeded):
		m.mu.Lock()
		m.awaitingPass = true
		m.mu.Unlock()

		res := SignInResult{NeedsPassword: true}
		if p, err := client.API().AccountGetPassword(ctx); err == nil {
			res.PasswordHint = p.Hint
		}
		return res, nil
	case err != nil:
		return SignInResult{}, fmt.Errorf("sign in: %w", friendly(err))
	}

	m.refreshAuth(ctx)
	return SignInResult{}, nil
}

// SubmitPassword completes a login on an account with 2FA enabled.
func (m *Manager) SubmitPassword(ctx context.Context, password string) error {
	client, err := m.Raw()
	if err != nil {
		return err
	}

	m.mu.RLock()
	awaiting := m.awaitingPass
	m.mu.RUnlock()
	if !awaiting {
		return errors.New("no password step is pending")
	}

	if _, err := client.Auth().Password(ctx, password); err != nil {
		return fmt.Errorf("submit password: %w", friendly(err))
	}
	m.refreshAuth(ctx)
	return nil
}

// LogOut ends the Telegram session and deletes the stored session file, so a
// later login starts clean rather than resurrecting a revoked session.
func (m *Manager) LogOut(ctx context.Context) error {
	client, err := m.Raw()
	if err == nil {
		if _, err := client.API().AuthLogOut(ctx); err != nil {
			m.log.Warn("telegram logout call failed; clearing local session anyway",
				zap.Error(err))
		}
	}

	m.Stop()
	if err := os.Remove(m.cfg.Telegram.SessionFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove session file: %w", err)
	}

	m.mu.Lock()
	m.loginPhone, m.loginCodeHash, m.awaitingPass = "", "", false
	m.mu.Unlock()

	// Reconnect unauthenticated so the wizard can immediately log in again.
	return m.Start(ctx)
}

// CancelLogin discards a half-finished login.
func (m *Manager) CancelLogin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loginPhone, m.loginCodeHash, m.awaitingPass = "", "", false
}

func describeCode(t tg.AuthSentCodeTypeClass) (CodeDelivery, int) {
	switch c := t.(type) {
	case *tg.AuthSentCodeTypeApp:
		return DeliveryApp, c.Length
	case *tg.AuthSentCodeTypeSMS:
		return DeliverySMS, c.Length
	case *tg.AuthSentCodeTypeCall:
		return DeliveryCall, c.Length
	case *tg.AuthSentCodeTypeFlashCall:
		return DeliveryFlashCall, 0
	case *tg.AuthSentCodeTypeMissedCall:
		return DeliveryMissed, c.Length
	case *tg.AuthSentCodeTypeEmailCode:
		return DeliveryEmail, c.Length
	case *tg.AuthSentCodeTypeFragmentSMS:
		return DeliveryFragment, c.Length
	default:
		return DeliveryOther, 0
	}
}

// normalisePhone strips the spaces, dashes and brackets people paste in, since
// Telegram wants a bare international number.
func normalisePhone(p string) string {
	var b strings.Builder
	for i, r := range strings.TrimSpace(p) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// friendly rewrites the handful of Telegram error codes a user can actually
// act on, and passes everything else through unchanged.
func friendly(err error) error {
	switch {
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return errors.New("that phone number is not valid")
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return errors.New("that login code is not correct")
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return errors.New("that login code has expired; request a new one")
	case tgerr.Is(err, "PASSWORD_HASH_INVALID"):
		return errors.New("that two-factor password is not correct")
	case tgerr.Is(err, "SESSION_PASSWORD_NEEDED"):
		return errors.New("this account needs its two-factor password")
	case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
		return errors.New("this phone number is banned from Telegram")
	case tgerr.Is(err, "API_ID_INVALID"):
		return errors.New("the api_id and api_hash pair is not valid")
	}
	if wait, ok := tgerr.AsFloodWait(err); ok {
		return fmt.Errorf("telegram asked us to wait %s before trying again", wait)
	}
	return err
}
