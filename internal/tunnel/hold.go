package tunnel

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const relayDialTimeout = 5 * time.Second

type Hold struct {
	ln      net.Listener
	mu      sync.Mutex
	backend int
	served  chan struct{}
}

const inUseProbe = 200 * time.Millisecond

func Listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil || port == 0 {
		return ln, err
	}
	if c, dialErr := net.DialTimeout("tcp", net.JoinHostPort("::1", strconv.Itoa(port)), inUseProbe); dialErr == nil { //nolint:gosec // a loopback address
		_ = c.Close()
		_ = ln.Close()
		return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, Err: syscall.EADDRINUSE}
	}
	return ln, nil
}

func InnerPort() (int, error) {
	ln, err := Listen(0)
	if err != nil {
		return 0, fmt.Errorf("find a port for the tunnel: %w\nFix: check that this machine can open local TCP ports", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func NewHold(ln net.Listener) *Hold {
	h := &Hold{ln: ln, served: make(chan struct{})}
	go h.serve()
	return h
}

func (h *Hold) Route(port int) {
	h.mu.Lock()
	h.backend = port
	h.mu.Unlock()
}

func (h *Hold) Close() error {
	err := h.ln.Close()
	<-h.served
	return err
}

func (h *Hold) route() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.backend
}

func (h *Hold) serve() {
	defer close(h.served)
	for {
		c, err := h.ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go h.relay(c)
	}
}

func (h *Hold) relay(c net.Conn) {
	defer c.Close()
	backend := h.route()
	if backend == 0 {
		return
	}
	up, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(backend)), relayDialTimeout)
	if err != nil {
		return
	}
	defer up.Close()
	copied := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if tc, ok := dst.(*net.TCPConn); ok && err == nil {
			_ = tc.CloseWrite()
		} else {
			_ = dst.Close()
			_ = src.Close()
		}
		copied <- struct{}{}
	}
	go pipe(up, c)
	go pipe(c, up)
	<-copied
	<-copied
}
