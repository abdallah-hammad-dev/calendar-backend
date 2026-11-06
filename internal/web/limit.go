package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Logins tried on the page, wrong or not yet known (security review, 2026-11-05). Its password is Davis's, which
// opens the calendar and, through the connector's consent, the vault. A second's wait per wrong try held nothing:
// tries sent side by side, each dropped after 50 ms, guessed as fast as the network went, and piled up in memory.
// Now each address gets perIP tries a window, and the whole page global an hour; past that, the answer is 429
// before the password is even compared, so going on teaches nothing. A good login gives its try back. The price:
// someone trying from many addresses locks me out of this page for up to an hour (CalDAV, LiveSync and the
// connector's tokens don't go through it).
const (
	perIP       = 10
	perIPWindow = 15 * time.Minute
	global      = 50
	globalSpan  = time.Hour
)

type limiter struct {
	mu     sync.Mutex
	all    []time.Time            // every try in the last globalSpan, oldest first
	byAddr map[string][]time.Time // the tries in the last perIPWindow, by address
	told   map[string]bool        // a limit already logged ("" = the global one)
}

// take counts a try from addr. ok false: refused, try again after wait. A try that turns out good is handed back
// with give(addr, at).
func (l *limiter) take(addr string, now time.Time) (at time.Time, wait time.Duration, ok bool, first bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byAddr == nil {
		l.byAddr = map[string][]time.Time{}
		l.told = map[string]bool{}
	}
	l.all = since(l.all, now.Add(-globalSpan))
	for a, tries := range l.byAddr {
		if tries = since(tries, now.Add(-perIPWindow)); len(tries) == 0 {
			delete(l.byAddr, a)
			delete(l.told, a)
		} else {
			l.byAddr[a] = tries
		}
	}
	if len(l.all) < global {
		delete(l.told, "")
	}
	mine := l.byAddr[addr]
	switch {
	case len(mine) >= perIP:
		first = !l.told[addr]
		l.told[addr] = true
		return time.Time{}, mine[0].Add(perIPWindow).Sub(now), false, first
	case len(l.all) >= global:
		first = !l.told[""]
		l.told[""] = true
		return time.Time{}, l.all[0].Add(globalSpan).Sub(now), false, first
	}
	l.all = append(l.all, now)
	l.byAddr[addr] = append(mine, now)
	return now, 0, true, false
}

func (l *limiter) give(addr string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = without(l.all, at)
	if tries := without(l.byAddr[addr], at); len(tries) == 0 {
		delete(l.byAddr, addr)
	} else {
		l.byAddr[addr] = tries
	}
}

func since(tries []time.Time, from time.Time) []time.Time {
	i := 0
	for i < len(tries) && !tries[i].After(from) {
		i++
	}
	return tries[i:]
}

func without(tries []time.Time, at time.Time) []time.Time {
	for i := len(tries) - 1; i >= 0; i-- {
		if tries[i].Equal(at) {
			return append(tries[:i:i], tries[i+1:]...)
		}
	}
	return tries
}

// clientAddr is who sent the request. Behind Caddy (the request comes from a private address) it is the last
// X-Forwarded-For entry, the one Caddy writes (it drops what clients send: no trusted_proxies). An IPv6 address
// counts as its /64: one machine gets a whole one.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err == nil && (ip.IsPrivate() || ip.IsLoopback()) {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			last := xff[len(xff)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if fwd, err := netip.ParseAddr(strings.TrimSpace(last)); err == nil {
				ip = fwd
			}
		}
	}
	if err != nil {
		return host
	}
	ip = ip.Unmap()
	if ip.Is6() {
		p, _ := ip.Prefix(64)
		return p.String()
	}
	return ip.String()
}
