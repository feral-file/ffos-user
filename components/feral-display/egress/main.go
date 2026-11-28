// ff-egress gives a worker in an isolated network namespace an HTTP proxy.
// Only the proxy can open Internet sockets. The inherited Unix control socket
// goes away with the worker, including when the host kills it during a cutover.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"

	"strconv"
	"strings"
	"syscall"
	"time"
)

const controlFD = 9

func socketpair(kind int) ([2]int, error) {
	return syscall.Socketpair(syscall.AF_UNIX, kind|syscall.SOCK_CLOEXEC, 0)
}

// Linux parent-death signals follow the thread that spawned the child.
// Keep launch and exec on the process leader, including across worker exec.

func daemon(mode string, files ...*os.File) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(self, mode)
	if mode == "relay" {
		child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	}
	// Player control pipes and compositor feedback belong only to the worker.
	// ExtraFiles explicitly remaps the broker channels needed by the helper.
	for fd := uintptr(3); fd <= 6; fd++ {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			if _, _, err := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, flags|syscall.FD_CLOEXEC); err != 0 {
				return err
			}
			defer syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_SETFD, flags)
		}
	}
	child.ExtraFiles = files
	child.Stderr = os.Stderr
	return child.Start()
}

func launch(mode string, args []string) error {
	if len(args) == 0 {
		return errors.New("missing worker command")
	}
	if mode == "host" || mode == "host-lan" {
		pair, err := socketpair(syscall.SOCK_SEQPACKET)
		if err != nil {
			return err
		}
		server := "serve"
		if mode == "host-lan" {
			server = "serve-lan"
		}
		if err = daemon(server, os.NewFile(uintptr(pair[0]), "broker")); err != nil {
			return err
		}
		if pair[1] != controlFD {
			if err = syscall.Dup3(pair[1], controlFD, 0); err != nil {
				return err
			}
		} else if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, controlFD, syscall.F_SETFD, 0); errno != 0 {
			return errno
		}
	} else {
		l, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 17873})
		if err != nil {
			return err
		}
		f, err := l.File()
		if err != nil {
			return err
		}
		if err = daemon("relay", os.NewFile(controlFD, "broker"), f); err != nil {
			return err
		}
		syscall.Close(controlFD)
	}
	return syscall.Exec(args[0], args, os.Environ())
}

var blocked = []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"}

func public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip)) {
		return false
	}
	for _, s := range blocked {
		if netip.MustParsePrefix(s).Contains(ip) {
			return false
		}
	}
	return true
}

func permittedRoute(raw []byte, allowLAN bool) bool {
	var routes []struct{ Dst, Type string }
	if json.Unmarshal(raw, &routes) != nil || len(routes) != 1 {
		return false
	}
	route := routes[0]
	if route.Type != "" && route.Type != "unicast" {
		return false
	}
	if route.Dst == "default" {
		return true
	}
	if !allowLAN {
		return false
	}
	_, prefixErr := netip.ParsePrefix(route.Dst)
	_, addrErr := netip.ParseAddr(route.Dst)
	return prefixErr == nil || addrErr == nil
}

// Set once in the broker process before serving its first request.
var allowLAN bool

func dial(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, errors.New("invalid port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		localName := !strings.Contains(host, ".") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".home.arpa")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") || (localName && !allowLAN) {
			return nil, errors.New("local name")
		}
		ips, err = (&net.Resolver{PreferGo: true}).LookupNetIP(ctx, "ip", host+".")
		if err != nil {
			return nil, err
		}
	}
	for _, ip := range ips {
		if !public(ip) && !(allowLAN && ip.Unmap().IsPrivate()) {
			return nil, errors.New("non-public address")
		}
	}
	for _, ip := range ips {
		// Check the matched FIB prefix. A more-specific route can lead to
		// another local subnet even when it goes through a gateway.
		raw, err := exec.CommandContext(ctx, "/usr/bin/ip", "-j", "route", "get", "fibmatch", ip.Unmap().String()).Output()
		if err != nil || !permittedRoute(raw, allowLAN) {
			continue
		}
		d := net.Dialer{Timeout: 8 * time.Second}
		if c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port)); err == nil {
			return c, nil
		}
	}
	return nil, errors.New("no permitted route")
}

func bridge(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 1)
	copyHalf := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if w, ok := dst.(interface{ CloseWrite() error }); ok {
			w.CloseWrite()
		}
	}
	go func() { copyHalf(a, b); done <- struct{}{} }()
	copyHalf(b, a)
	<-done
}

var transport = &http.Transport{DialContext: dial, DisableCompression: true, MaxIdleConns: 16, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 30 * time.Second}

func proxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		dst, err := dial(ctx, "tcp", r.Host)
		if err != nil {
			http.Error(w, "Destination denied or unavailable", 502)
			return
		}
		c, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			dst.Close()
			return
		}
		if _, err = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			c.Close()
			dst.Close()
			return
		}
		if n := buf.Reader.Buffered(); n > 0 {
			io.CopyN(dst, buf.Reader, int64(n))
		}
		bridge(c, dst)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		http.Error(w, "Expected HTTP proxy request", 400)
		return
	}
	proxyHTTP(w, r)
}

type channels struct{ fd int }

func (l channels) Accept() (net.Conn, error) {
	var request [1]byte
	n, err := syscall.Read(l.fd, request[:])
	for err == syscall.EINTR {
		n, err = syscall.Read(l.fd, request[:])
	}
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, io.EOF
	}
	p, err := socketpair(syscall.SOCK_STREAM)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(p[1])
	f := os.NewFile(uintptr(p[0]), "stream")
	defer f.Close()
	if err = syscall.Sendmsg(l.fd, request[:], syscall.UnixRights(p[1]), nil, 0); err != nil {
		return nil, err
	}
	return net.FileConn(f)
}
func (l channels) Close() error   { return syscall.Close(l.fd) }
func (l channels) Addr() net.Addr { return &net.UnixAddr{Name: "artwork", Net: "unix"} }

func relay() error {
	l, err := net.FileListener(os.NewFile(4, "listener"))
	if err != nil {
		return err
	}
	slots := make(chan struct{}, 64)
	for {
		slots <- struct{}{}
		c, err := l.Accept()
		if err != nil {
			return err
		}
		if _, err = syscall.Write(3, []byte{1}); err != nil {
			return err
		}
		ancillary := make([]byte, syscall.CmsgSpace(4))
		_, n, _, _, err := syscall.Recvmsg(3, make([]byte, 1), ancillary, syscall.MSG_CMSG_CLOEXEC)
		if err != nil {
			return err
		}
		msgs, err := syscall.ParseSocketControlMessage(ancillary[:n])
		if err != nil || len(msgs) != 1 {
			return errors.New("broker disconnected")
		}
		fds, err := syscall.ParseUnixRights(&msgs[0])
		if err != nil || len(fds) != 1 {
			return errors.New("invalid broker response")
		}
		f := os.NewFile(uintptr(fds[0]), "stream")
		dst, err := net.FileConn(f)
		f.Close()
		if err != nil {
			return err
		}
		go func() { defer func() { <-slots }(); bridge(c, dst) }()
	}
}

func run(args []string) {
	if len(args) < 2 {
		log.Fatal("missing mode")
	}
	var err error
	switch args[1] {
	case "host", "host-lan", "inside":
		err = launch(args[1], args[2:])
	case "serve", "serve-lan":
		allowLAN = args[1] == "serve-lan"
		err = (&http.Server{Handler: http.HandlerFunc(proxy), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 65536}).Serve(channels{3})
	case "relay":
		err = relay()
	default:
		err = fmt.Errorf("unknown mode")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		log.Fatal(err)
	}
}
