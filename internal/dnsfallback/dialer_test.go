package dnsfallback

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeConn struct {
	net.Conn
	remote net.Addr
}

func (c fakeConn) RemoteAddr() net.Addr { return c.remote }
func (c fakeConn) Close() error         { return nil }

func TestFallsBackToLastAddressWhenDNSFails(t *testing.T) {
	d := New()
	now := time.Unix(1_000_000, 0)
	d.now = func() time.Time { return now }
	dnsDown := false
	var dialed []string
	d.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		host, _, _ := net.SplitHostPort(addr)
		if net.ParseIP(host) == nil {
			if dnsDown {
				return nil, &net.OpError{Op: "dial", Err: &net.DNSError{Err: "server misbehaving", Name: host}}
			}
			return fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("2607:6bc0::10"), Port: 443}}, nil
		}
		return fakeConn{remote: &net.TCPAddr{IP: net.ParseIP(host), Port: 443}}, nil
	}
	if _, err := d.DialContext(context.Background(), "tcp", "api.example.com:443"); err != nil {
		t.Fatal(err)
	}
	dnsDown = true
	dialed = nil
	if _, err := d.DialContext(context.Background(), "tcp", "api.example.com:443"); err != nil {
		t.Fatalf("fallback failed: %v", err)
	}
	if len(dialed) != 2 || dialed[1] != "[2607:6bc0::10]:443" {
		t.Fatalf("dialed %v", dialed)
	}
	// Past maxAge the remembered address is no longer used.
	now = now.Add(maxAge + time.Minute)
	if _, err := d.DialContext(context.Background(), "tcp", "api.example.com:443"); err == nil {
		t.Fatal("expected the DNS error after maxAge")
	}
}

func TestNonDNSErrorIsReturned(t *testing.T) {
	d := New()
	boom := errors.New("refused")
	d.hosts["h"] = []seen{{ip: "1.2.3.4", at: time.Now()}}
	calls := 0
	d.dial = func(context.Context, string, string) (net.Conn, error) { calls++; return nil, boom }
	if _, err := d.DialContext(context.Background(), "tcp", "h:443"); !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestRecentPrefersIPv4(t *testing.T) {
	d := New()
	now := time.Now()
	d.hosts["h"] = []seen{{ip: "2607::1", at: now}, {ip: "1.2.3.4", at: now}}
	if got := d.recent("h"); got[0] != "1.2.3.4" {
		t.Fatalf("got %v", got)
	}
}
