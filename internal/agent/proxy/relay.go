package proxy

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Relays cover a loopback family the runtime does not publish. Colima forwards
// unprivileged ports to 127.0.0.1 only (over ssh), so [::1]:18443 would stay free for
// any program to take, and clients try ::1 first (P-6). The agent then listens on
// [::1]:port itself and passes connections to 127.0.0.1:port. It needs no root for
// ports ≥ 1024; below that the runtimes listen on both families anyway.
type Relays struct {
	mu     sync.Mutex
	active map[netip.AddrPort]*relay
}

type relay struct {
	ln net.Listener
	to netip.AddrPort
}

// Ensure makes the relays match ports: one for every unprivileged port whose IPv6
// loopback nobody serves, none for other ports. It returns what is relayed.
func (rs *Relays) Ensure(ports Ports) []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.active == nil {
		rs.active = map[netip.AddrPort]*relay{}
	}
	want := map[netip.AddrPort]netip.AddrPort{}
	for _, p := range []uint16{ports.HTTP, ports.HTTPS} {
		if p >= 1024 {
			want[netip.AddrPortFrom(netip.IPv6Loopback(), p)] = netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), p)
		}
	}
	for from, r := range rs.active {
		if _, ok := want[from]; !ok {
			r.ln.Close()
			delete(rs.active, from)
		}
	}
	for from, to := range want {
		if _, ok := rs.active[from]; ok {
			continue
		}
		// A bind that fails means someone serves it: the runtime itself (Docker
		// Desktop publishes ::1) or, if it was free when the proxy started, nobody new.
		ln, err := net.Listen("tcp6", from.String())
		if err != nil {
			continue
		}
		r := &relay{ln: ln, to: to}
		rs.active[from] = r
		go r.serve()
	}
	var out []string
	for from, r := range rs.active {
		out = append(out, fmt.Sprintf("%s → %s", from, r.to))
	}
	sort.Strings(out)
	return out
}

// Close stops every relay.
func (rs *Relays) Close() { rs.Ensure(Ports{}) }

func (r *relay) serve() {
	for {
		c, err := r.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go r.pipe(c)
	}
}

func (r *relay) pipe(in net.Conn) {
	defer in.Close()
	out, err := net.DialTimeout("tcp", r.to.String(), 5*time.Second)
	if err != nil {
		return
	}
	defer out.Close()
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(out, in)
	go cp(in, out)
	<-done
	<-done
}
