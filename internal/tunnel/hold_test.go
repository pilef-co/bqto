package tunnel

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func echoServer(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr).Port
}

func roundTrip(t *testing.T, port int) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping\n")); err != nil {
		return "", err
	}
	return bufio.NewReader(c).ReadString('\n')
}

func TestHoldKeepsThePortWhileTheTunnelIsDown(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHold(ln)
	defer h.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if _, err := roundTrip(t, port); err == nil {
		t.Error("a connection before the tunnel is up reached something; want it closed")
	}

	first, firstPort := echoServer(t)
	h.Route(firstPort)
	if got, err := roundTrip(t, port); err != nil || got != "ping\n" {
		t.Fatalf("through the first tunnel: %q, %v", got, err)
	}

	first.Close()
	h.Route(0)
	if other, err := Listen(port); err == nil {
		other.Close()
		t.Fatalf("another program bound port %d while the tunnel was down", port)
	}
	if _, err := roundTrip(t, port); err == nil {
		t.Error("a connection while the tunnel is down reached something; want it closed")
	}

	second, secondPort := echoServer(t)
	defer second.Close()
	h.Route(secondPort)
	if got, err := roundTrip(t, port); err != nil || got != "ping\n" {
		t.Fatalf("through the reconnected tunnel: %q, %v", got, err)
	}
}

func TestHoldFreesThePortWhenClosed(t *testing.T) {
	ln, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := NewHold(ln).Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Listen(port)
	if err != nil {
		t.Fatalf("port %d still held after Close: %v", port, err)
	}
	again.Close()
}

func TestInnerPortIsFree(t *testing.T) {
	p, err := InnerPort()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(p)
	if err != nil {
		t.Fatalf("InnerPort returned %d, which is not free: %v", p, err)
	}
	ln.Close()
}

func TestListenRefusesAPortAnotherProgramAnswersOnIPv6(t *testing.T) {
	other, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback")
	}
	defer other.Close()
	port := other.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := other.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if ln, err := Listen(port); err == nil {
		_ = ln.Close()
		t.Fatalf("Listen(%d) succeeded while [::1]:%d answers", port, port)
	} else if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("Listen(%d): %v; want EADDRINUSE", port, err)
	}
	if got, err := FreePort(port); err != nil || got == port {
		t.Errorf("FreePort(%d) = %d, %v; want another port", port, got, err)
	}
}
