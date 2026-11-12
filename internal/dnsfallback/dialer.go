// Package dnsfallback provides a direct dialer that remembers the addresses it last connected
// to, so a flaky local DNS resolver does not fail requests to hosts it reached recently.
package dnsfallback

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// maxAge bounds how long a remembered address is reused after DNS starts failing.
	maxAge = 24 * time.Hour
	// maxAddrs is how many distinct addresses are remembered per host.
	maxAddrs = 4
	// warnGap spaces out the warning logged while the fallback is in use.
	warnGap = time.Minute
)

type seen struct {
	ip string
	at time.Time
}

// Dialer dials like net.Dialer and, when the name lookup fails, retries with the addresses it
// connected to for that host within maxAge. It implements proxy.Dialer and proxy.ContextDialer.
type Dialer struct {
	base     *net.Dialer
	mu       sync.Mutex
	hosts    map[string][]seen
	lastWarn time.Time
	now      func() time.Time
	dial     func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New returns a Dialer with net.Dialer's usual timeouts.
func New() *Dialer {
	d := &Dialer{base: &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, hosts: make(map[string][]seen)}
	d.dial = d.base.DialContext
	return d
}

var shared = New()

// Shared returns the process-wide Dialer, so every transport shares the remembered addresses.
func Shared() *Dialer { return shared }

// Dial implements proxy.Dialer.
func (d *Dialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

// DialContext implements proxy.ContextDialer.
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, errSplit := net.SplitHostPort(addr)
	if errSplit != nil || net.ParseIP(host) != nil {
		return d.dial(ctx, network, addr)
	}
	conn, err := d.dial(ctx, network, addr)
	if err == nil {
		d.remember(host, conn.RemoteAddr())
		return conn, nil
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || ctx.Err() != nil {
		return nil, err
	}
	ips := d.recent(host)
	if len(ips) == 0 {
		return nil, err
	}
	d.warn(host, dnsErr)
	for _, ip := range ips {
		c, errIP := d.dial(ctx, network, net.JoinHostPort(ip, port))
		if errIP == nil {
			return c, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, err
}

func (d *Dialer) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Dialer) remember(host string, remote net.Addr) {
	tcp, ok := remote.(*net.TCPAddr)
	if !ok || tcp.IP == nil {
		return
	}
	ip, now := tcp.IP.String(), d.clock()
	d.mu.Lock()
	defer d.mu.Unlock()
	list := []seen{{ip: ip, at: now}}
	for _, s := range d.hosts[host] {
		if s.ip != ip && len(list) < maxAddrs {
			list = append(list, s)
		}
	}
	d.hosts[host] = list
}

// recent returns the remembered addresses for host that are younger than maxAge, IPv4 first
// (a broken IPv6 route is a common companion of a broken resolver), newest first within a family.
func (d *Dialer) recent(host string) []string {
	now := d.clock()
	d.mu.Lock()
	defer d.mu.Unlock()
	var v4, v6 []string
	for _, s := range d.hosts[host] {
		if now.Sub(s.at) > maxAge {
			continue
		}
		if ip := net.ParseIP(s.ip); ip != nil && ip.To4() != nil {
			v4 = append(v4, s.ip)
		} else {
			v6 = append(v6, s.ip)
		}
	}
	return append(v4, v6...)
}

func (d *Dialer) warn(host string, err error) {
	d.mu.Lock()
	now := d.clock()
	if now.Sub(d.lastWarn) < warnGap {
		d.mu.Unlock()
		return
	}
	d.lastWarn = now
	d.mu.Unlock()
	log.Warnf("dns lookup for %s failed (%v); using its last known addresses", host, err)
}
