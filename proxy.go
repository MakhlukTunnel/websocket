package websocket

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Proxy Dialer interface
type proxyDialer interface {
	Dial(network, addr string) (net.Conn, error)
}

// SOCKS5 proxy implementation
func proxySOCKS5(network, addr string, auth *proxyAuth, forward proxyDialer) (proxyDialer, error) {
	s := &socks5Proxy{
		network: network,
		addr:    addr,
		forward: forward,
	}
	if auth != nil {
		s.user = auth.User
		s.password = auth.Password
	}
	return s, nil
}

type socks5Proxy struct {
	user, password string
	network, addr  string
	forward        proxyDialer
}

const (
	socks5Version = 5
	socks5AuthNone = 0
	socks5AuthPassword = 2
	socks5Connect = 1
	socks5IP4 = 1
	socks5Domain = 3
	socks5IP6 = 4
)

var socks5Errors = []string{
	"",
	"general failure",
	"connection forbidden",
	"network unreachable",
	"host unreachable",
	"connection refused",
	"TTL expired",
	"command not supported",
	"address type not supported",
}

func (s *socks5Proxy) Dial(network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp6", "tcp4":
	default:
		return nil, errors.New("proxy: no support for SOCKS5 proxy connections of type " + network)
	}

	conn, err := s.forward.Dial(s.network, s.addr)
	if err != nil {
		return nil, err
	}
	if err := s.connect(conn, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (s *socks5Proxy) connect(conn net.Conn, target string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return err
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return errors.New("proxy: failed to parse port number: " + portStr)
	}
	if port < 1 || port > 0xffff {
		return errors.New("proxy: port number out of range: " + portStr)
	}

	// Authentication negotiation
	buf := make([]byte, 0, 6+len(host))
	buf = append(buf, socks5Version)
	
	if len(s.user) > 0 && len(s.user) < 256 && len(s.password) < 256 {
		buf = append(buf, 2, socks5AuthNone, socks5AuthPassword)
	} else {
		buf = append(buf, 1, socks5AuthNone)
	}

	if _, err := conn.Write(buf); err != nil {
		return errors.New("proxy: failed to write greeting to SOCKS5 proxy: " + err.Error())
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return errors.New("proxy: failed to read greeting from SOCKS5 proxy: " + err.Error())
	}
	
	if buf[0] != 5 {
		return errors.New("proxy: SOCKS5 proxy has unexpected version")
	}
	if buf[1] == 0xff {
		return errors.New("proxy: SOCKS5 proxy requires authentication")
	}

	// Password authentication if required
	if buf[1] == socks5AuthPassword {
		buf = buf[:0]
		buf = append(buf, 1)
		buf = append(buf, uint8(len(s.user)))
		buf = append(buf, s.user...)
		buf = append(buf, uint8(len(s.password)))
		buf = append(buf, s.password...)

		if _, err := conn.Write(buf); err != nil {
			return errors.New("proxy: failed to write authentication request: " + err.Error())
		}

		if _, err := io.ReadFull(conn, buf[:2]); err != nil {
			return errors.New("proxy: failed to read authentication reply: " + err.Error())
		}
		if buf[1] != 0 {
			return errors.New("proxy: SOCKS5 proxy rejected username/password")
		}
	}

	// Connection request
	buf = buf[:0]
	buf = append(buf, socks5Version, socks5Connect, 0)

	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			buf = append(buf, socks5IP4)
			ip = ip4
		} else {
			buf = append(buf, socks5IP6)
		}
		buf = append(buf, ip...)
	} else {
		if len(host) > 255 {
			return errors.New("proxy: destination host name too long")
		}
		buf = append(buf, socks5Domain)
		buf = append(buf, byte(len(host)))
		buf = append(buf, host...)
	}
	buf = append(buf, byte(port>>8), byte(port))

	if _, err := conn.Write(buf); err != nil {
		return errors.New("proxy: failed to write connect request: " + err.Error())
	}

	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return errors.New("proxy: failed to read connect reply: " + err.Error())
	}

	failure := "unknown error"
	if int(buf[1]) < len(socks5Errors) {
		failure = socks5Errors[buf[1]]
	}
	if len(failure) > 0 {
		return errors.New("proxy: SOCKS5 proxy failed to connect: " + failure)
	}

	// Discard remaining address data
	bytesToDiscard := 0
	switch buf[3] {
	case socks5IP4:
		bytesToDiscard = net.IPv4len
	case socks5IP6:
		bytesToDiscard = net.IPv6len
	case socks5Domain:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return errors.New("proxy: failed to read domain length: " + err.Error())
		}
		bytesToDiscard = int(buf[0])
	default:
		return errors.New("proxy: got unknown address type")
	}

	if cap(buf) < bytesToDiscard {
		buf = make([]byte, bytesToDiscard)
	} else {
		buf = buf[:bytesToDiscard]
	}
	if _, err := io.ReadFull(conn, buf); err != nil {
		return errors.New("proxy: failed to read address: " + err.Error())
	}
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return errors.New("proxy: failed to read port: " + err.Error())
	}

	return nil
}

// HTTP Proxy implementation
type httpProxyDialer struct {
	proxyURL    *url.URL
	forwardDial func(network, addr string) (net.Conn, error)
}

func (hpd *httpProxyDialer) Dial(network string, addr string) (net.Conn, error) {
	hostPort, _ := hostPortNoPort(hpd.proxyURL)
	conn, err := hpd.forwardDial(network, hostPort)
	if err != nil {
		return nil, err
	}

	connectHeader := make(http.Header)
	if user := hpd.proxyURL.User; user != nil {
		proxyUser := user.Username()
		if proxyPassword, passwordSet := user.Password(); passwordSet {
			credential := base64.StdEncoding.EncodeToString([]byte(proxyUser + ":" + proxyPassword))
			connectHeader.Set("Proxy-Authorization", "Basic "+credential)
		}
	}

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: connectHeader,
	}

	if err := connectReq.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		conn.Close()
		return nil, err
	}

	if resp.StatusCode != 200 {
		conn.Close()
		f := strings.SplitN(resp.Status, " ", 2)
		return nil, errors.New(f[1])
	}
	return conn, nil
}

// Environment variable proxy support
type proxyAuth struct {
	User, Password string
}

type proxyEnvOnce struct {
	names []string
	once  sync.Once
	val   string
}

func (e *proxyEnvOnce) Get() string {
	e.once.Do(e.init)
	return e.val
}

func (e *proxyEnvOnce) init() {
	for _, n := range e.names {
		e.val = os.Getenv(n)
		if e.val != "" {
			return
		}
	}
}

var (
	allProxyEnv = &proxyEnvOnce{names: []string{"ALL_PROXY", "all_proxy"}}
	noProxyEnv  = &proxyEnvOnce{names: []string{"NO_PROXY", "no_proxy"}}
)

type proxyDirect struct{}

func (proxyDirect) Dial(network, addr string) (net.Conn, error) {
	return net.Dial(network, addr)
}

var directProxy = proxyDirect{}

type proxyPerHost struct {
	def, bypass proxyDialer
	bypassNetworks []*net.IPNet
	bypassIPs      []net.IP
	bypassZones    []string
	bypassHosts    []string
}

func newProxyPerHost(defaultDialer, bypass proxyDialer) *proxyPerHost {
	return &proxyPerHost{
		def:    defaultDialer,
		bypass: bypass,
	}
}

func (p *proxyPerHost) Dial(network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	return p.dialerForRequest(host).Dial(network, addr)
}

func (p *proxyPerHost) dialerForRequest(host string) proxyDialer {
	if ip := net.ParseIP(host); ip != nil {
		for _, net := range p.bypassNetworks {
			if net.Contains(ip) {
				return p.bypass
			}
		}
		for _, bypassIP := range p.bypassIPs {
			if bypassIP.Equal(ip) {
				return p.bypass
			}
		}
		return p.def
	}

	for _, zone := range p.bypassZones {
		if strings.HasSuffix(host, zone) {
			return p.bypass
		}
		if host == zone[1:] {
			return p.bypass
		}
	}
	for _, bypassHost := range p.bypassHosts {
		if bypassHost == host {
			return p.bypass
		}
	}
	return p.def
}

func (p *proxyPerHost) AddFromString(s string) {
	hosts := strings.Split(s, ",")
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if len(host) == 0 {
			continue
		}
		if strings.Contains(host, "/") {
			if _, net, err := net.ParseCIDR(host); err == nil {
				p.AddNetwork(net)
			}
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			p.AddIP(ip)
			continue
		}
		if strings.HasPrefix(host, "*.") {
			p.AddZone(host[1:])
			continue
		}
		p.AddHost(host)
	}
}

func (p *proxyPerHost) AddIP(ip net.IP) {
	p.bypassIPs = append(p.bypassIPs, ip)
}

func (p *proxyPerHost) AddNetwork(net *net.IPNet) {
	p.bypassNetworks = append(p.bypassNetworks, net)
}

func (p *proxyPerHost) AddZone(zone string) {
	if strings.HasSuffix(zone, ".") {
		zone = zone[:len(zone)-1]
	}
	if !strings.HasPrefix(zone, ".") {
		zone = "." + zone
	}
	p.bypassZones = append(p.bypassZones, zone)
}

func (p *proxyPerHost) AddHost(host string) {
	if strings.HasSuffix(host, ".") {
		host = host[:len(host)-1]
	}
	p.bypassHosts = append(p.bypassHosts, host)
}

func proxyFromEnvironment() proxyDialer {
	allProxy := allProxyEnv.Get()
	if len(allProxy) == 0 {
		return directProxy
	}

	proxyURL, err := url.Parse(allProxy)
	if err != nil {
		return directProxy
	}
	
	proxy, err := proxyFromURL(proxyURL, directProxy)
	if err != nil {
		return directProxy
	}

	noProxy := noProxyEnv.Get()
	if len(noProxy) == 0 {
		return proxy
	}

	perHost := newProxyPerHost(proxy, directProxy)
	perHost.AddFromString(noProxy)
	return perHost
}

var proxySchemes map[string]func(*url.URL, proxyDialer) (proxyDialer, error)

func registerProxyDialerType(scheme string, f func(*url.URL, proxyDialer) (proxyDialer, error)) {
	if proxySchemes == nil {
		proxySchemes = make(map[string]func(*url.URL, proxyDialer) (proxyDialer, error))
	}
	proxySchemes[scheme] = f
}

func proxyFromURL(u *url.URL, forward proxyDialer) (proxyDialer, error) {
	var auth *proxyAuth
	if u.User != nil {
		auth = &proxyAuth{
			User: u.User.Username(),
		}
		if p, ok := u.User.Password(); ok {
			auth.Password = p
		}
	}

	switch u.Scheme {
	case "socks5":
		return proxySOCKS5("tcp", u.Host, auth, forward)
	case "http":
		return &httpProxyDialer{proxyURL: u, forwardDial: forward.Dial}, nil
	}

	if proxySchemes != nil {
		if f, ok := proxySchemes[u.Scheme]; ok {
			return f(u, forward)
		}
	}
	return nil, errors.New("proxy: unknown scheme: " + u.Scheme)
}

func init() {
	registerProxyDialerType("http", func(proxyURL *url.URL, forwardDialer proxyDialer) (proxyDialer, error) {
		return &httpProxyDialer{proxyURL: proxyURL, forwardDial: forwardDialer.Dial}, nil
	})
}