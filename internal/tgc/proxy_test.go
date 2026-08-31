package tgc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
)

func TestNormalizeProxyURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "bare address defaults to socks5", input: "127.0.0.1:1080", want: "socks5://127.0.0.1:1080"},
		{
			name:  "socks5 credentials",
			input: "socks5://alice:secret@proxy.example:1080",
			want:  "socks5://alice:secret@proxy.example:1080",
		},
		{name: "http", input: "http://proxy.example:8080", want: "http://proxy.example:8080"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := NormalizeProxyURL(test.input)
			if err != nil {
				t.Fatalf("NormalizeProxyURL returned an error: %v", err)
			}
			if got != test.want {
				t.Fatalf("NormalizeProxyURL(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestNormalizeProxyURLRejectsNonAddressSuffixes(t *testing.T) {
	for _, input := range []string{
		"ftp://proxy.example:21",
		"http://proxy.example",
		"http://proxy.example:8080/path",
		"http://proxy.example:8080?token=secret",
		"http://proxy.example:8080#fragment",
	} {
		if _, err := NormalizeProxyURL(input); !errors.Is(err, ErrBadProxy) {
			t.Errorf("NormalizeProxyURL(%q) error = %v, want ErrBadProxy", input, err)
		}
	}
}

func TestMaskProxyURLDoesNotExposePassword(t *testing.T) {
	masked := MaskProxyURL("http://alice:secret@proxy.example:8080")
	if strings.Contains(masked, "secret") {
		t.Fatalf("MaskProxyURL exposed the password: %q", masked)
	}
	if !strings.Contains(masked, "alice") || !strings.Contains(masked, "***") {
		t.Fatalf("MaskProxyURL(%q) = %q, want the user and a masked password", "http://alice:secret@proxy.example:8080", masked)
	}
}

func TestHTTPConnectDialerWritesAuthenticatedConnect(t *testing.T) {
	clientConnection, proxyConnection := net.Pipe()
	defer clientConnection.Close()
	defer proxyConnection.Close()

	dialer := httpConnectDialer{
		address: "proxy.example:8080",
		auth:    "Basic dXNlcjpwYXNz",
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- dialer.writeConnect(proxyConnection, "149.154.167.51:443")
	}()

	request, err := http.ReadRequest(bufio.NewReader(clientConnection))
	if err != nil {
		t.Fatalf("read CONNECT request: %v", err)
	}
	if request.Method != "CONNECT" {
		t.Fatalf("request method = %q, want CONNECT", request.Method)
	}
	if request.Host != "149.154.167.51:443" {
		t.Fatalf("request host = %q, want Telegram's address", request.Host)
	}
	if request.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
		t.Fatalf("proxy authorization = %q, want the configured value", request.Header.Get("Proxy-Authorization"))
	}

	if _, err := clientConnection.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT response: %v", err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("writeConnect returned an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the CONNECT response to be processed")
	}
}

func TestTelegramResolverUsesOneDialerForEveryDCPath(t *testing.T) {
	var addresses []string
	dial := func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("resolver requested network %q, want tcp", network)
		}
		addresses = append(addresses, address)
		return &discardConn{}, nil
	}

	resolver := newTelegramResolver(dial)
	options := dcsTestOptions()

	connection, err := resolver.Primary(context.Background(), 1, options)
	if err != nil {
		t.Fatalf("Primary: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close Primary connection: %v", err)
	}

	connection, err = resolver.MediaOnly(context.Background(), 2, options)
	if err != nil {
		t.Fatalf("MediaOnly: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close MediaOnly connection: %v", err)
	}

	connection, err = resolver.CDN(context.Background(), 3, options)
	if err != nil {
		t.Fatalf("CDN: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close CDN connection: %v", err)
	}

	want := []string{"primary.example:443", "media.example:443", "cdn.example:443"}
	if !slicesEqual(addresses, want) {
		t.Fatalf("resolver dial addresses = %v, want %v", addresses, want)
	}
}

func dcsTestOptions() dcs.List {
	return dcs.List{Options: []tg.DCOption{
		{ID: 1, IPAddress: "primary.example", Port: 443},
		{ID: 2, IPAddress: "media.example", Port: 443, MediaOnly: true},
		{ID: 3, IPAddress: "cdn.example", Port: 443, CDN: true},
	}}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type discardConn struct{}

func (discardConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (discardConn) Write(buffer []byte) (int, error) { return len(buffer), nil }
func (discardConn) Close() error                     { return nil }
func (discardConn) LocalAddr() net.Addr              { return discardAddr("local") }
func (discardConn) RemoteAddr() net.Addr             { return discardAddr("remote") }
func (discardConn) SetDeadline(time.Time) error      { return nil }
func (discardConn) SetReadDeadline(time.Time) error  { return nil }
func (discardConn) SetWriteDeadline(time.Time) error { return nil }

type discardAddr string

func (address discardAddr) Network() string { return "discard" }
func (address discardAddr) String() string  { return string(address) }

var _ net.Conn = discardConn{}
