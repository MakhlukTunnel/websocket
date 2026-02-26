package websocket

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"
)

// Errors
var (
	ErrBadHandshake        = errors.New("websocket: bad handshake")
	errInvalidCompression  = errors.New("websocket: invalid compression negotiation")
	errMalformedURL        = errors.New("malformed ws or wss URL")
)

// Dialer contains options for connecting to WebSocket server
type Dialer struct {
	NetDial            func(network, addr string) (net.Conn, error)
	Method             string
	NetDialContext     func(ctx context.Context, network, addr string) (net.Conn, error)
	NetDialTLSContext  func(ctx context.Context, network, addr string) (net.Conn, error)
	Proxy              func(*http.Request) (*url.URL, error)
	TLSClientConfig    *tls.Config
	HandshakeTimeout   time.Duration
	ReadBufferSize     int
	WriteBufferSize    int
	WriteBufferPool    BufferPool
	Subprotocols       []string
	EnableCompression  bool
	Jar                http.CookieJar
}

// DefaultDialer is a dialer with default values
var DefaultDialer = &Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 45 * time.Second,
}

var nilDialer = *DefaultDialer

// Dial creates a new client connection
func (d *Dialer) Dial(urlStr string, requestHeader http.Header) (*Conn, *http.Response, error) {
	return d.DialContext(context.Background(), urlStr, requestHeader)
}

// DialContext creates a new client connection with context
func (d *Dialer) DialContext(ctx context.Context, urlStr string, requestHeader http.Header) (*Conn, *http.Response, error) {
	if d == nil {
		d = &nilDialer
	}

	challengeKey, err := generateChallengeKey()
	if err != nil {
		return nil, nil, err
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, nil, err
	}

	// Normalize scheme
	switch u.Scheme {
	case "http", "ws":
		u.Scheme = "http"
	case "https", "wss":
		u.Scheme = "https"
	default:
		return nil, nil, errMalformedURL
	}

	if u.User != nil {
		return nil, nil, errMalformedURL
	}

	// Parse custom method and path
	if d.Method == "" {
		d.Method = "GET"
	}

	pathUnescape, _ := url.QueryUnescape(u.Path)
	pathTrimmed := strings.TrimPrefix(pathUnescape, "/")
	pathReplace := strings.Replace(pathTrimmed, " ", ":", 1)
	pathSplited := strings.Split(pathReplace, ":")
	
	if len(pathSplited) == 2 {
		u.Opaque = pathTrimmed
	} else if len(pathSplited) == 3 {
		d.Method = pathSplited[0]
		u.Opaque = pathSplited[1] + ":" + pathSplited[2]
	}

	// Build request
	req := &http.Request{
		Method:     d.Method,
		URL:        u,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
		Host:       u.Host,
	}
	req = req.WithContext(ctx)

	// Add cookies
	if d.Jar != nil {
		for _, cookie := range d.Jar.Cookies(u) {
			req.AddCookie(cookie)
		}
	}

	// Set WebSocket headers
	setWebSocketHeaders(req, challengeKey, d.Subprotocols, d.EnableCompression)

	// Merge request headers
	if err := mergeHeaders(req, requestHeader, d.Subprotocols); err != nil {
		return nil, nil, err
	}

	// Setup timeout
	if d.HandshakeTimeout != 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, d.HandshakeTimeout)
		defer cancel()
	}

	// Get network dial function
	netDial := getDialFunc(ctx, d, u.Scheme)

	// Handle proxy
	netDial, err = wrapProxy(netDial, d.Proxy, req)
	if err != nil {
		return nil, nil, err
	}

	hostPort, hostNoPort := hostPortNoPort(u)
	
	// Dial
	trace := httptrace.ContextClientTrace(ctx)
	if trace != nil && trace.GetConn != nil {
		trace.GetConn(hostPort)
	}

	netConn, err := netDial("tcp", hostPort)
	if trace != nil && trace.GotConn != nil {
		trace.GotConn(httptrace.GotConnInfo{Conn: netConn})
	}
	if err != nil {
		return nil, nil, err
	}

	defer func() {
		if netConn != nil {
			netConn.Close()
		}
	}()

	// TLS handshake if needed
	netConn, err = handleTLS(ctx, netConn, u.Scheme, d, hostNoPort, trace)
	if err != nil {
		return nil, nil, err
	}

	// Create connection
	conn := newConn(netConn, false, d.ReadBufferSize, d.WriteBufferSize, d.WriteBufferPool, nil, nil)

	// Send handshake request
	if err := req.Write(netConn); err != nil {
		return nil, nil, err
	}

	// Trace first response byte
	if trace != nil && trace.GotFirstResponseByte != nil {
		if peek, err := conn.br.Peek(1); err == nil && len(peek) == 1 {
			trace.GotFirstResponseByte()
		}
	}

	// Read response
	resp, err := http.ReadResponse(conn.br, req)
	if err != nil {
		return nil, nil, err
	}

	// Save cookies
	if d.Jar != nil {
		if rc := resp.Cookies(); len(rc) > 0 {
			d.Jar.SetCookies(u, rc)
		}
	}

	// Validate handshake
	if err := validateHandshake(resp, challengeKey); err != nil {
		// Save response body for debugging
		buf := make([]byte, 1024)
		n, _ := io.ReadFull(resp.Body, buf)
		resp.Body = ioutil.NopCloser(bytes.NewReader(buf[:n]))
		return nil, resp, err
	}

	// Setup compression
	setupCompression(conn, resp)

	resp.Body = ioutil.NopCloser(bytes.NewReader([]byte{}))
	conn.subprotocol = resp.Header.Get("Sec-Websocket-Protocol")

	netConn.SetDeadline(time.Time{})
	netConn = nil
	return conn, resp, nil
}

// Helper functions
func setWebSocketHeaders(req *http.Request, challengeKey string, subprotocols []string, enableCompression bool) {
	req.Header["Upgrade"] = []string{"websocket"}
	req.Header["Connection"] = []string{"Upgrade"}
	req.Header["Sec-WebSocket-Key"] = []string{challengeKey}
	req.Header["Sec-WebSocket-Version"] = []string{"13"}
	
	if len(subprotocols) > 0 {
		req.Header["Sec-WebSocket-Protocol"] = []string{strings.Join(subprotocols, ", ")}
	}
	
	if enableCompression {
		req.Header["Sec-WebSocket-Extensions"] = []string{"permessage-deflate; server_no_context_takeover; client_no_context_takeover"}
	}
}

func mergeHeaders(req *http.Request, requestHeader http.Header, subprotocols []string) error {
	for k, vs := range requestHeader {
		switch {
		case k == "Host":
			if len(vs) > 0 {
				req.Host = vs[0]
			}
		case k == "Upgrade" || k == "Connection" || k == "Sec-Websocket-Key" ||
			k == "Sec-Websocket-Version" || k == "Sec-Websocket-Extensions" ||
			(k == "Sec-Websocket-Protocol" && len(subprotocols) > 0):
			return errors.New("websocket: duplicate header not allowed: " + k)
		case k == "Sec-Websocket-Protocol":
			req.Header["Sec-WebSocket-Protocol"] = vs
		default:
			req.Header[k] = vs
		}
	}
	return nil
}

func getDialFunc(ctx context.Context, d *Dialer, scheme string) func(string, string) (net.Conn, error) {
	switch scheme {
	case "http":
		if d.NetDialContext != nil {
			return func(network, addr string) (net.Conn, error) {
				return d.NetDialContext(ctx, network, addr)
			}
		}
		if d.NetDial != nil {
			return d.NetDial
		}
	case "https":
		if d.NetDialTLSContext != nil {
			return func(network, addr string) (net.Conn, error) {
				return d.NetDialTLSContext(ctx, network, addr)
			}
		}
		if d.NetDialContext != nil {
			return func(network, addr string) (net.Conn, error) {
				return d.NetDialContext(ctx, network, addr)
			}
		}
		if d.NetDial != nil {
			return d.NetDial
		}
	}

	// Default dialer
	netDialer := &net.Dialer{}
	return func(network, addr string) (net.Conn, error) {
		return netDialer.DialContext(ctx, network, addr)
	}
}

func wrapProxy(netDial func(string, string) (net.Conn, error), proxyFunc func(*http.Request) (*url.URL, error), 
	req *http.Request) (func(string, string) (net.Conn, error), error) {
	
	if proxyFunc == nil {
		return netDial, nil
	}

	proxyURL, err := proxyFunc(req)
	if err != nil {
		return nil, err
	}
	if proxyURL == nil {
		return netDial, nil
	}

	dialer, err := proxyFromURL(proxyURL, netDialerFunc(netDial))
	if err != nil {
		return nil, err
	}
	return dialer.Dial, nil
}

func handleTLS(ctx context.Context, netConn net.Conn, scheme string, d *Dialer, 
	hostNoPort string, trace *httptrace.ClientTrace) (net.Conn, error) {
	
	if scheme != "https" || d.NetDialTLSContext != nil {
		return netConn, nil
	}

	cfg := cloneTLSConfig(d.TLSClientConfig)
	if cfg.ServerName == "" {
		cfg.ServerName = hostNoPort
	}
	tlsConn := tls.Client(netConn, cfg)

	if trace != nil && trace.TLSHandshakeStart != nil {
		trace.TLSHandshakeStart()
	}
	err := doHandshake(ctx, tlsConn, cfg)
	if trace != nil && trace.TLSHandshakeDone != nil {
		trace.TLSHandshakeDone(tlsConn.ConnectionState(), err)
	}
	if err != nil {
		return nil, err
	}
	return tlsConn, nil
}

func validateHandshake(resp *http.Response, challengeKey string) error {
	if resp.StatusCode == 101 &&
		tokenListContainsValue(resp.Header, "Upgrade", "websocket") &&
		tokenListContainsValue(resp.Header, "Connection", "upgrade") &&
		resp.Header.Get("Sec-Websocket-Accept") == computeAcceptKey(challengeKey) {
		return nil
	}
	return ErrBadHandshake
}

func setupCompression(conn *Conn, resp *http.Response) {
	for _, ext := range parseExtensions(resp.Header) {
		if ext[""] != "permessage-deflate" {
			continue
		}
		_, snct := ext["server_no_context_takeover"]
		_, cnct := ext["client_no_context_takeover"]
		if !snct || !cnct {
			continue
		}
		conn.newCompressionWriter = compressNoContextTakeover
		conn.newDecompressionReader = decompressNoContextTakeover
		break
	}
}

func hostPortNoPort(u *url.URL) (hostPort, hostNoPort string) {
	hostPort = u.Host
	hostNoPort = u.Host
	if i := strings.LastIndex(u.Host, ":"); i > strings.LastIndex(u.Host, "]") {
		hostNoPort = hostNoPort[:i]
	} else {
		switch u.Scheme {
		case "wss", "https":
			hostPort += ":443"
		default:
			hostPort += ":80"
		}
	}
	return hostPort, hostNoPort
}

func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return cfg.Clone()
}

// Deprecated: Use Dialer instead
func NewClient(netConn net.Conn, u *url.URL, requestHeader http.Header, 
	readBufSize, writeBufSize int) (*Conn, *http.Response, error) {
	d := Dialer{
		ReadBufferSize:  readBufSize,
		WriteBufferSize: writeBufSize,
		NetDial: func(net, addr string) (net.Conn, error) {
			return netConn, nil
		},
	}
	return d.Dial(u.String(), requestHeader)
}

type netDialerFunc func(network, addr string) (net.Conn, error)

func (fn netDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return fn(network, addr)
}