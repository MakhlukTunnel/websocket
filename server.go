package websocket

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HandshakeError describes an error with the handshake
type HandshakeError struct {
	message string
}

func (e HandshakeError) Error() string { return e.message }

// Upgrader specifies parameters for upgrading an HTTP connection
type Upgrader struct {
	HandshakeTimeout   time.Duration
	ReadBufferSize     int
	WriteBufferSize    int
	WriteBufferPool    BufferPool
	Subprotocols       []string
	Error              func(w http.ResponseWriter, r *http.Request, status int, reason error)
	CheckOrigin        func(r *http.Request) bool
	EnableCompression  bool
}

// Upgrade upgrades the HTTP server connection to WebSocket
func (u *Upgrader) Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header) (*Conn, error) {
	const badHandshake = "websocket: the client is not using the websocket protocol: "

	// Validate handshake
	if err := u.validateHandshake(r, responseHeader); err != nil {
		return u.returnError(w, r, http.StatusBadRequest, err.Error())
	}

	// Check origin
	if !u.checkOrigin(r) {
		return u.returnError(w, r, http.StatusForbidden, "websocket: request origin not allowed")
	}

	// Get challenge key
	challengeKey := r.Header.Get("Sec-Websocket-Key")
	if challengeKey == "" {
		return u.returnError(w, r, http.StatusBadRequest, 
			"websocket: not a websocket handshake: 'Sec-WebSocket-Key' header is missing")
	}

	// Negotiate subprotocol
	subprotocol := u.selectSubprotocol(r, responseHeader)

	// Negotiate compression
	compress := u.negotiateCompression(r)

	// Hijack connection
	h, ok := w.(http.Hijacker)
	if !ok {
		return u.returnError(w, r, http.StatusInternalServerError, 
			"websocket: response does not implement http.Hijacker")
	}
	
	netConn, brw, err := h.Hijack()
	if err != nil {
		return u.returnError(w, r, http.StatusInternalServerError, err.Error())
	}

	if brw.Reader.Buffered() > 0 {
		netConn.Close()
		return nil, errors.New("websocket: client sent data before handshake is complete")
	}

	// Setup buffers
	var br *bufio.Reader
	if u.ReadBufferSize == 0 && bufioReaderSize(netConn, brw.Reader) > 256 {
		br = brw.Reader
	}

	buf := bufioWriterBuffer(netConn, brw.Writer)
	var writeBuf []byte
	if u.WriteBufferPool == nil && u.WriteBufferSize == 0 && len(buf) >= maxFrameHeaderSize+256 {
		writeBuf = buf
	}

	// Create connection
	c := newConn(netConn, true, u.ReadBufferSize, u.WriteBufferSize, u.WriteBufferPool, br, writeBuf)
	c.subprotocol = subprotocol

	if compress {
		c.newCompressionWriter = compressNoContextTakeover
		c.newDecompressionReader = decompressNoContextTakeover
	}

	// Build response
	response := u.buildResponse(challengeKey, subprotocol, compress, responseHeader)

	// Clear deadlines
	netConn.SetDeadline(time.Time{})

	// Send response
	if u.HandshakeTimeout > 0 {
		netConn.SetWriteDeadline(time.Now().Add(u.HandshakeTimeout))
	}
	if _, err = netConn.Write(response); err != nil {
		netConn.Close()
		return nil, err
	}
	if u.HandshakeTimeout > 0 {
		netConn.SetWriteDeadline(time.Time{})
	}

	return c, nil
}

func (u *Upgrader) validateHandshake(r *http.Request, responseHeader http.Header) error {
	if !tokenListContainsValue(r.Header, "Connection", "upgrade") {
		return HandshakeError{"'upgrade' token not found in 'Connection' header"}
	}
	if !tokenListContainsValue(r.Header, "Upgrade", "websocket") {
		return HandshakeError{"'websocket' token not found in 'Upgrade' header"}
	}
	if !tokenListContainsValue(r.Header, "Sec-Websocket-Version", "13") {
		return HandshakeError{"unsupported version: 13 not found in 'Sec-Websocket-Version' header"}
	}
	if responseHeader != nil {
		if _, ok := responseHeader["Sec-Websocket-Extensions"]; ok {
			return HandshakeError{"application specific 'Sec-WebSocket-Extensions' headers are unsupported"}
		}
	}
	return nil
}

func (u *Upgrader) checkOrigin(r *http.Request) bool {
	if u.CheckOrigin == nil {
		return checkSameOrigin(r)
	}
	return u.CheckOrigin(r)
}

func (u *Upgrader) returnError(w http.ResponseWriter, r *http.Request, status int, reason string) (*Conn, error) {
	err := HandshakeError{reason}
	if u.Error != nil {
		u.Error(w, r, status, err)
	} else {
		w.Header().Set("Sec-Websocket-Version", "13")
		http.Error(w, http.StatusText(status), status)
	}
	return nil, err
}

func (u *Upgrader) selectSubprotocol(r *http.Request, responseHeader http.Header) string {
	if u.Subprotocols != nil {
		clientProtocols := Subprotocols(r)
		for _, serverProtocol := range u.Subprotocols {
			for _, clientProtocol := range clientProtocols {
				if clientProtocol == serverProtocol {
					return clientProtocol
				}
			}
		}
	} else if responseHeader != nil {
		return responseHeader.Get("Sec-Websocket-Protocol")
	}
	return ""
}

func (u *Upgrader) negotiateCompression(r *http.Request) bool {
	if !u.EnableCompression {
		return false
	}
	for _, ext := range parseExtensions(r.Header) {
		if ext[""] == "permessage-deflate" {
			return true
		}
	}
	return false
}

func (u *Upgrader) buildResponse(challengeKey, subprotocol string, compress bool, 
	responseHeader http.Header) []byte {
	
	// Use larger buffer
	p := make([]byte, 0, 1024)

	// Status line
	p = append(p, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "...)
	p = append(p, computeAcceptKey(challengeKey)...)
	p = append(p, "\r\n"...)

	// Subprotocol
	if subprotocol != "" {
		p = append(p, "Sec-WebSocket-Protocol: "...)
		p = append(p, subprotocol...)
		p = append(p, "\r\n"...)
	}

	// Compression
	if compress {
		p = append(p, "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"...)
	}

	// Custom headers
	if responseHeader != nil {
		for k, vs := range responseHeader {
			if k == "Sec-Websocket-Protocol" {
				continue
			}
			for _, v := range vs {
				p = append(p, k...)
				p = append(p, ": "...)
				for i := 0; i < len(v); i++ {
					b := v[i]
					if b <= 31 {
						b = ' '
					}
					p = append(p, b)
				}
				p = append(p, "\r\n"...)
			}
		}
	}
	p = append(p, "\r\n"...)
	return p
}

func checkSameOrigin(r *http.Request) bool {
	origin := r.Header["Origin"]
	if len(origin) == 0 {
		return true
	}
	u, err := url.Parse(origin[0])
	if err != nil {
		return false
	}
	return equalASCIIFold(u.Host, r.Host)
}

// Subprotocols returns the subprotocols requested by the client
func Subprotocols(r *http.Request) []string {
	h := strings.TrimSpace(r.Header.Get("Sec-Websocket-Protocol"))
	if h == "" {
		return nil
	}
	protocols := strings.Split(h, ",")
	for i := range protocols {
		protocols[i] = strings.TrimSpace(protocols[i])
	}
	return protocols
}

// IsWebSocketUpgrade returns true if the client requested upgrade
func IsWebSocketUpgrade(r *http.Request) bool {
	return tokenListContainsValue(r.Header, "Connection", "upgrade") &&
		tokenListContainsValue(r.Header, "Upgrade", "websocket")
}

// Deprecated: Use Upgrader instead
func Upgrade(w http.ResponseWriter, r *http.Request, responseHeader http.Header, 
	readBufSize, writeBufSize int) (*Conn, error) {
	u := Upgrader{
		ReadBufferSize:  readBufSize,
		WriteBufferSize: writeBufSize,
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	return u.Upgrade(w, r, responseHeader)
}

// Buffer size utilities
func bufioReaderSize(originalReader io.Reader, br *bufio.Reader) int {
	br.Reset(originalReader)
	if p, err := br.Peek(0); err == nil {
		return cap(p)
	}
	return 0
}

type writeHook struct{ p []byte }

func (wh *writeHook) Write(p []byte) (int, error) {
	wh.p = p
	return len(p), nil
}

func bufioWriterBuffer(originalWriter io.Writer, bw *bufio.Writer) []byte {
	var wh writeHook
	bw.Reset(&wh)
	bw.WriteByte(0)
	bw.Flush()
	bw.Reset(originalWriter)
	return wh.p[:cap(wh.p)]
}