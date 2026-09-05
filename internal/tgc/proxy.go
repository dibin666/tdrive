package tgc

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	"github.com/gotd/td/telegram/dcs"
)

// Per-account outbound proxies.
//
// Telegram does not only meter per account; it also notices that several
// accounts sign in and work from one address, and treats them as one operator.
// That is how a drive with two accounts ends up with both of them rate limited
// at once, which defeats the entire reason for adding the second. Giving each
// account its own exit is the fix, and it has to be per account rather than
// per process, because a single shared proxy would just move all of them
// behind one address again.
//
// The proxy applies to the MTProto connections themselves, so it covers
// everything an account does — login, uploads, downloads — from the first code
// request onwards. That first request matters most: an account whose login
// came from the shared address is already associated before it has stored a
// single byte.

// ErrBadProxy marks a proxy address that cannot be used, either because it does
// not parse or because nothing answers on it. It is a configuration mistake
// rather than an outage, so the API maps it to a 4xx.
var ErrBadProxy = errors.New("tgc: this proxy address cannot be used")

// proxyDialTimeout bounds one connection attempt through a proxy. MTProto
// reconnects on its own, so a hung proxy has to fail rather than block the
// dialler indefinitely.
const proxyDialTimeout = 20 * time.Second

// NormalizeProxyURL validates an operator-supplied proxy address and returns the
// canonical form to store.
//
// A bare "host:port" is accepted and read as SOCKS5, because that is the form
// every proxy vendor prints and the one people paste. Everything else has to
// name its scheme, since socks5 and http need different handshakes and guessing
// wrong produces a connection that fails much later for no obvious reason.
func NormalizeProxyURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "socks5://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadProxy, err)
	}
	switch parsed.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return "", fmt.Errorf("%w: %q is not a supported kind of proxy (use socks5, http or https)",
			ErrBadProxy, parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", fmt.Errorf("%w: the address has no host", ErrBadProxy)
	}
	if parsed.Port() == "" {
		return "", fmt.Errorf("%w: the address has no port, for example socks5://127.0.0.1:1080", ErrBadProxy)
	}

	// Only the parts that address the proxy are kept. A path or a query string
	// means the operator pasted something that is not a proxy address — a
	// subscription link, usually — and storing it would fail at dial time with
	// a far less helpful message.
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("%w: a proxy address has no path, only host and port", ErrBadProxy)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%w: a proxy address has no query or fragment", ErrBadProxy)
	}
	canonical := url.URL{Scheme: parsed.Scheme, User: parsed.User, Host: parsed.Host}
	return canonical.String(), nil
}

// MaskProxyURL renders a proxy address for display with its password removed.
// The accounts list is polled by every open settings tab, so the stored
// credential must not travel with it.
func MaskProxyURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(代理地址无效)"
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			parsed.User = url.UserPassword(parsed.User.Username(), "***")
		}
	}
	// url.URL.String percent-encodes the masked password into %2A%2A%2A, which
	// is unreadable in a list; the point here is legibility, not a valid URL.
	return strings.ReplaceAll(parsed.String(), "%2A%2A%2A", "***")
}

// proxyResolver builds the DC resolver for an account, which is what makes
// every MTProto connection it opens leave through its own proxy. The resolver
// is used for gotd's primary, media-only and CDN connection paths; the same
// object is retained by gotd when it creates the main client, a connection
// pool, or a connection for another datacenter. An empty address means a
// direct connection, and returns nil so gotd keeps its default.
func proxyResolver(raw string) (dcs.Resolver, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dial, err := proxyDialFunc(raw)
	if err != nil {
		return nil, err
	}
	return newTelegramResolver(dial), nil
}

// newTelegramResolver creates the one resolver used for every kind of
// Telegram datacenter connection. Keeping this construction in one function
// prevents a future media or CDN path from accidentally falling back to a
// direct dialer while the primary path still uses the account proxy.
func newTelegramResolver(dial dcs.DialFunc) dcs.Resolver {
	return dcs.Plain(dcs.PlainOptions{
		Dial:    dial,
		Network: "tcp",
	})
}

// proxyDialFunc turns a stored proxy address into the dial function gotd uses
// for every datacenter connection.
func proxyDialFunc(raw string) (dcs.DialFunc, error) {
	canonical, err := NormalizeProxyURL(raw)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(canonical)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadProxy, err)
	}

	forward := &net.Dialer{Timeout: proxyDialTimeout, KeepAlive: 30 * time.Second}
	switch parsed.Scheme {
	case "socks5", "socks5h":
		dialer, err := proxy.SOCKS5("tcp", parsed.Host, socksAuth(parsed.User), forward)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadProxy, err)
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("%w: this SOCKS5 dialer cannot honour cancellation", ErrBadProxy)
		}
		return contextDialer.DialContext, nil
	case "http", "https":
		return httpConnectDialer{
			address: parsed.Host,
			// A proxy reached over https terminates TLS itself, so the CONNECT
			// request has to be written inside that TLS session rather than in
			// the clear.
			useTLS:  parsed.Scheme == "https",
			auth:    basicProxyAuth(parsed.User),
			forward: forward,
		}.DialContext, nil
	}
	return nil, fmt.Errorf("%w: %q is not a supported kind of proxy", ErrBadProxy, parsed.Scheme)
}

func socksAuth(userInfo *url.Userinfo) *proxy.Auth {
	if userInfo == nil {
		return nil
	}
	password, _ := userInfo.Password()
	return &proxy.Auth{User: userInfo.Username(), Password: password}
}

func basicProxyAuth(userInfo *url.Userinfo) string {
	if userInfo == nil {
		return ""
	}
	password, _ := userInfo.Password()
	request := http.Request{Header: http.Header{}}
	request.SetBasicAuth(userInfo.Username(), password)
	return request.Header.Get("Authorization")
}

// httpConnectDialer tunnels a TCP connection through an HTTP proxy with the
// CONNECT method, which is the only thing an HTTP proxy can do for a protocol
// it does not understand — and MTProto is emphatically one of those.
//
// net/http's own transport does this internally but will not hand back the raw
// tunnelled connection, so the handshake is written out here.
type httpConnectDialer struct {
	address string
	useTLS  bool
	// auth is a ready-made Proxy-Authorization header value, empty when the
	// proxy needs no credentials.
	auth    string
	forward *net.Dialer
}

func (d httpConnectDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("an HTTP proxy cannot carry %q connections", network)
	}

	conn, err := d.forward.DialContext(ctx, "tcp", d.address)
	if err != nil {
		return nil, fmt.Errorf("reach the proxy at %s: %w", d.address, err)
	}
	tunnelled := false
	defer func() {
		if !tunnelled {
			_ = conn.Close()
		}
	}()

	// The handshake is plain blocking I/O, so cancellation has to be expressed
	// as a deadline on the connection itself.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(proxyDialTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	if d.useTLS {
		host, _, splitErr := net.SplitHostPort(d.address)
		if splitErr != nil {
			host = d.address
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("negotiate TLS with the proxy at %s: %w", d.address, err)
		}
		conn = tlsConn
	}

	if err := d.writeConnect(conn, target); err != nil {
		return nil, err
	}

	tunnelled = true
	// The deadline covered the handshake only. Leaving it in place would kill
	// the MTProto connection at that instant, which is a long-lived one.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// writeConnect performs the CONNECT exchange and reports whether the proxy
// agreed to carry the connection.
func (d httpConnectDialer) writeConnect(conn net.Conn, target string) error {
	request := &http.Request{
		Method: http.MethodConnect,
		// Opaque rather than Path: a CONNECT request line carries the
		// authority alone, with no scheme and no slash.
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{},
	}
	if d.auth != "" {
		request.Header.Set("Proxy-Authorization", d.auth)
	}
	if err := request.Write(conn); err != nil {
		return fmt.Errorf("send CONNECT to the proxy at %s: %w", d.address, err)
	}

	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return fmt.Errorf("read the proxy's answer from %s: %w", d.address, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: the proxy at %s refused to connect to %s (%s)",
			ErrBadProxy, d.address, target, response.Status)
	}
	// Everything after the response belongs to the tunnel, and it is read
	// straight from the connection rather than through this reader. A proxy
	// that spoke first would therefore lose those bytes, so refuse instead of
	// corrupting the MTProto stream.
	if reader.Buffered() > 0 {
		return fmt.Errorf("%w: the proxy at %s sent data before the tunnel was open",
			ErrBadProxy, d.address)
	}
	return nil
}

// CheckProxy confirms that a proxy is usable before it is stored, by asking it
// to reach Telegram.
//
// Storing an address that does not work would take the account offline until
// somebody noticed, and the failure would surface as a reconnect loop rather
// than as an answer to the action that caused it. Checking first turns that
// into an error message on the form.
func CheckProxy(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	dial, err := proxyDialFunc(raw)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, proxyDialTimeout)
	defer cancel()

	conn, err := dial(ctx, "tcp", telegramProbeAddress)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadProxy, err)
	}
	return conn.Close()
}

// telegramProbeAddress is a Telegram datacenter, used only to prove that the
// proxy will carry a connection to Telegram specifically — a proxy that is up
// but blocks Telegram is a failure worth catching here rather than at login.
const telegramProbeAddress = "149.154.167.51:443"
