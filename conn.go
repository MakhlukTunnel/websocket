// Copyright 2013 The Gorilla WebSocket Authors. All rights reserved.
package websocket

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// Constants
const (
	finalBit = 1 << 7
	rsv1Bit  = 1 << 6
	rsv2Bit  = 1 << 5
	rsv3Bit  = 1 << 4
	maskBit  = 1 << 7

	maxFrameHeaderSize         = 2 + 8 + 4
	maxControlFramePayloadSize = 125
	writeWait                  = time.Second
	defaultReadBufferSize      = 4096
	defaultWriteBufferSize     = 4096

	continuationFrame = 0
	noFrame           = -1

	// Message Types
	TextMessage   = 1
	BinaryMessage = 2
	CloseMessage  = 8
	PingMessage   = 9
	PongMessage   = 10

	// Close Codes
	CloseNormalClosure           = 1000
	CloseGoingAway               = 1001
	CloseProtocolError           = 1002
	CloseUnsupportedData         = 1003
	CloseNoStatusReceived        = 1005
	CloseAbnormalClosure         = 1006
	CloseInvalidFramePayloadData = 1007
	ClosePolicyViolation         = 1008
	CloseMessageTooBig           = 1009
	CloseMandatoryExtension      = 1010
	CloseInternalServerErr       = 1011
	CloseServiceRestart          = 1012
	CloseTryAgainLater           = 1013
	CloseTLSHandshake            = 1015
)

// Errors
var (
	ErrCloseSent            = errors.New("websocket: close sent")
	ErrReadLimit            = errors.New("websocket: read limit exceeded")
	errWriteTimeout         = &netError{msg: "websocket: write timeout", timeout: true, temporary: true}
	errUnexpectedEOF        = &CloseError{Code: CloseAbnormalClosure, Text: io.ErrUnexpectedEOF.Error()}
	errBadWriteOpCode       = errors.New("websocket: bad write message type")
	errWriteClosed          = errors.New("websocket: write closed")
	errInvalidControlFrame  = errors.New("websocket: invalid control frame")
)

type netError struct {
	msg       string
	temporary bool
	timeout   bool
}

func (e *netError) Error() string   { return e.msg }
func (e *netError) Temporary() bool { return e.temporary }
func (e *netError) Timeout() bool   { return e.timeout }

// CloseError represents a close message
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	s := []byte("websocket: close ")
	s = strconv.AppendInt(s, int64(e.Code), 10)
	if e.Text != "" {
		s = append(s, ": "...)
		s = append(s, e.Text...)
	}
	return string(s)
}

// BufferPool represents a pool of buffers
type BufferPool interface {
	Get() interface{}
	Put(interface{})
}

type writePoolData struct{ buf []byte }

// Conn represents a WebSocket connection
type Conn struct {
	conn        net.Conn
	isServer    bool
	subprotocol string

	// Write fields
	mu                     chan struct{}
	writeBuf               []byte
	writePool              BufferPool
	writeBufSize           int
	writeDeadline          time.Time
	writer                 io.WriteCloser
	isWriting              bool
	writeErrMu             sync.Mutex
	writeErr               error
	enableWriteCompression bool
	compressionLevel       int
	newCompressionWriter   func(io.WriteCloser, int) io.WriteCloser

	// Read fields
	reader                 io.ReadCloser
	readErr                error
	br                     *bufio.Reader
	readRemaining          int64
	readFinal              bool
	readLength             int64
	readLimit              int64
	readMaskPos            int
	readMaskKey            [4]byte
	handlePong             func(string) error
	handlePing             func(string) error
	handleClose            func(int, string) error
	readErrCount           int
	messageReader          *messageReader
	readDecompress         bool
	newDecompressionReader func(io.Reader) io.ReadCloser
}

func newConn(conn net.Conn, isServer bool, readBufferSize, writeBufferSize int, 
	writeBufferPool BufferPool, br *bufio.Reader, writeBuf []byte) *Conn {
	
	if br == nil {
		if readBufferSize == 0 {
			readBufferSize = defaultReadBufferSize
		} else if readBufferSize < maxControlFramePayloadSize {
			readBufferSize = maxControlFramePayloadSize
		}
		br = bufio.NewReaderSize(conn, readBufferSize)
	}

	if writeBufferSize <= 0 {
		writeBufferSize = defaultWriteBufferSize
	}
	writeBufferSize += maxFrameHeaderSize

	if writeBuf == nil && writeBufferPool == nil {
		writeBuf = make([]byte, writeBufferSize)
	}

	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	
	c := &Conn{
		isServer:               isServer,
		br:                     br,
		conn:                   conn,
		mu:                     mu,
		readFinal:              true,
		writeBuf:               writeBuf,
		writePool:              writeBufferPool,
		writeBufSize:           writeBufferSize,
		enableWriteCompression: true,
		compressionLevel:       1,
	}
	c.SetCloseHandler(nil)
	c.SetPingHandler(nil)
	c.SetPongHandler(nil)
	return c
}

// Subprotocol returns the negotiated protocol
func (c *Conn) Subprotocol() string { return c.subprotocol }

// Close closes the connection
func (c *Conn) Close() error { return c.conn.Close() }

// LocalAddr returns local network address
func (c *Conn) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// RemoteAddr returns remote network address
func (c *Conn) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// UnderlyingConn returns the internal net.Conn
func (c *Conn) UnderlyingConn() net.Conn { return c.conn }

// SetReadLimit sets maximum message size
func (c *Conn) SetReadLimit(limit int64) { c.readLimit = limit }

// SetWriteDeadline sets write deadline
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return nil
}

// SetReadDeadline sets read deadline
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// EnableWriteCompression enables/disables write compression
func (c *Conn) EnableWriteCompression(enable bool) {
	c.enableWriteCompression = enable
}

// SetCompressionLevel sets compression level
func (c *Conn) SetCompressionLevel(level int) error {
	if !isValidCompressionLevel(level) {
		return errors.New("websocket: invalid compression level")
	}
	c.compressionLevel = level
	return nil
}

// CloseHandler returns close handler
func (c *Conn) CloseHandler() func(int, string) error { return c.handleClose }

// SetCloseHandler sets close handler
func (c *Conn) SetCloseHandler(h func(int, string) error) {
	if h == nil {
		h = func(code int, text string) error {
			message := FormatCloseMessage(code, "")
			c.WriteControl(CloseMessage, message, time.Now().Add(writeWait))
			return nil
		}
	}
	c.handleClose = h
}

// PingHandler returns ping handler
func (c *Conn) PingHandler() func(string) error { return c.handlePing }

// SetPingHandler sets ping handler
func (c *Conn) SetPingHandler(h func(string) error) {
	if h == nil {
		h = func(message string) error {
			err := c.WriteControl(PongMessage, []byte(message), time.Now().Add(writeWait))
			if err == ErrCloseSent {
				return nil
			}
			if e, ok := err.(net.Error); ok && e.Temporary() {
				return nil
			}
			return err
		}
	}
	c.handlePing = h
}

// PongHandler returns pong handler
func (c *Conn) PongHandler() func(string) error { return c.handlePong }

// SetPongHandler sets pong handler
func (c *Conn) SetPongHandler(h func(string) error) {
	if h == nil {
		h = func(string) error { return nil }
	}
	c.handlePong = h
}

// WriteControl writes a control message
func (c *Conn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	if !isControl(messageType) {
		return errBadWriteOpCode
	}
	if len(data) > maxControlFramePayloadSize {
		return errInvalidControlFrame
	}

	b0 := byte(messageType) | finalBit
	b1 := byte(len(data))
	if !c.isServer {
		b1 |= maskBit
	}

	buf := make([]byte, 0, maxFrameHeaderSize+maxControlFramePayloadSize)
	buf = append(buf, b0, b1)

	if c.isServer {
		buf = append(buf, data...)
	} else {
		key := newMaskKey()
		buf = append(buf, key[:]...)
		buf = append(buf, data...)
		maskBytes(key, 0, buf[6:])
	}

	d := 1000 * time.Hour
	if !deadline.IsZero() {
		d = deadline.Sub(time.Now())
		if d < 0 {
			return errWriteTimeout
		}
	}

	timer := time.NewTimer(d)
	select {
	case <-c.mu:
		timer.Stop()
	case <-timer.C:
		return errWriteTimeout
	}
	defer func() { c.mu <- struct{}{} }()

	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}

	c.conn.SetWriteDeadline(deadline)
	_, err = c.conn.Write(buf)
	if err != nil {
		return c.writeFatal(err)
	}
	if messageType == CloseMessage {
		c.writeFatal(ErrCloseSent)
	}
	return err
}

// NextWriter returns a writer for the next message
func (c *Conn) NextWriter(messageType int) (io.WriteCloser, error) {
	var mw messageWriter
	if err := c.beginMessage(&mw, messageType); err != nil {
		return nil, err
	}
	c.writer = &mw
	if c.newCompressionWriter != nil && c.enableWriteCompression && isData(messageType) {
		w := c.newCompressionWriter(c.writer, c.compressionLevel)
		mw.compress = true
		c.writer = w
	}
	return c.writer, nil
}

// WriteMessage writes a message
func (c *Conn) WriteMessage(messageType int, data []byte) error {
	if c.isServer && (c.newCompressionWriter == nil || !c.enableWriteCompression) {
		var mw messageWriter
		if err := c.beginMessage(&mw, messageType); err != nil {
			return err
		}
		n := copy(c.writeBuf[mw.pos:], data)
		mw.pos += n
		return mw.flushFrame(true, data[n:])
	}

	w, err := c.NextWriter(messageType)
	if err != nil {
		return err
	}
	if _, err = w.Write(data); err != nil {
		return err
	}
	return w.Close()
}

// WritePreparedMessage writes prepared message
func (c *Conn) WritePreparedMessage(pm *PreparedMessage) error {
	frameType, frameData, err := pm.frame(prepareKey{
		isServer:         c.isServer,
		compress:         c.newCompressionWriter != nil && c.enableWriteCompression && isData(pm.messageType),
		compressionLevel: c.compressionLevel,
	})
	if err != nil {
		return err
	}
	if c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = true
	err = c.write(frameType, c.writeDeadline, frameData, nil)
	if !c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = false
	return err
}

// NextReader returns the next data message
func (c *Conn) NextReader() (messageType int, r io.Reader, err error) {
	if c.reader != nil {
		c.reader.Close()
		c.reader = nil
	}

	c.messageReader = nil
	c.readLength = 0

	for c.readErr == nil {
		frameType, err := c.advanceFrame()
		if err != nil {
			c.readErr = hideTempErr(err)
			break
		}

		if frameType == TextMessage || frameType == BinaryMessage {
			c.messageReader = &messageReader{c}
			c.reader = c.messageReader
			if c.readDecompress {
				c.reader = c.newDecompressionReader(c.reader)
			}
			return frameType, c.reader, nil
		}
	}

	c.readErrCount++
	if c.readErrCount >= 1000 {
		panic("repeated read on failed websocket connection")
	}
	return noFrame, nil, c.readErr
}

// ReadMessage reads a message
func (c *Conn) ReadMessage() (messageType int, p []byte, err error) {
	var r io.Reader
	messageType, r, err = c.NextReader()
	if err != nil {
		return messageType, nil, err
	}
	p, err = ioutil.ReadAll(r)
	return messageType, p, err
}

// Internal methods
func (c *Conn) writeFatal(err error) error {
	err = hideTempErr(err)
	c.writeErrMu.Lock()
	if c.writeErr == nil {
		c.writeErr = err
	}
	c.writeErrMu.Unlock()
	return err
}

func (c *Conn) read(n int) ([]byte, error) {
	p, err := c.br.Peek(n)
	if err == io.EOF {
		err = errUnexpectedEOF
	}
	c.br.Discard(len(p))
	return p, err
}

func (c *Conn) write(frameType int, deadline time.Time, buf0, buf1 []byte) error {
	<-c.mu
	defer func() { c.mu <- struct{}{} }()

	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}

	c.conn.SetWriteDeadline(deadline)
	if len(buf1) == 0 {
		_, err = c.conn.Write(buf0)
	} else {
		err = c.writeBufs(buf0, buf1)
	}
	if err != nil {
		return c.writeFatal(err)
	}
	if frameType == CloseMessage {
		c.writeFatal(ErrCloseSent)
	}
	return nil
}

func (c *Conn) writeBufs(bufs ...[]byte) error {
	b := net.Buffers(bufs)
	_, err := b.WriteTo(c.conn)
	return err
}

func (c *Conn) setReadRemaining(n int64) error {
	if n < 0 {
		return ErrReadLimit
	}
	c.readRemaining = n
	return nil
}

func (c *Conn) beginMessage(mw *messageWriter, messageType int) error {
	if c.writer != nil {
		c.writer.Close()
		c.writer = nil
	}

	if !isControl(messageType) && !isData(messageType) {
		return errBadWriteOpCode
	}

	c.writeErrMu.Lock()
	err := c.writeErr
	c.writeErrMu.Unlock()
	if err != nil {
		return err
	}

	mw.c = c
	mw.frameType = messageType
	mw.pos = maxFrameHeaderSize

	if c.writeBuf == nil {
		wpd, ok := c.writePool.Get().(writePoolData)
		if ok {
			c.writeBuf = wpd.buf
		} else {
			c.writeBuf = make([]byte, c.writeBufSize)
		}
	}
	return nil
}

func (c *Conn) advanceFrame() (int, error) {
	if c.readRemaining > 0 {
		if _, err := io.CopyN(ioutil.Discard, c.br, c.readRemaining); err != nil {
			return noFrame, err
		}
	}

	p, err := c.read(2)
	if err != nil {
		return noFrame, err
	}

	frameType := int(p[0] & 0xf)
	final := p[0]&finalBit != 0
	rsv1 := p[0]&rsv1Bit != 0
	rsv2 := p[0]&rsv2Bit != 0
	rsv3 := p[0]&rsv3Bit != 0
	mask := p[1]&maskBit != 0
	c.setReadRemaining(int64(p[1] & 0x7f))

	c.readDecompress = false
	if rsv1 {
		if c.newDecompressionReader != nil {
			c.readDecompress = true
		} else {
			return noFrame, c.handleProtocolError("RSV1 set")
		}
	}

	if rsv2 {
		return noFrame, c.handleProtocolError("RSV2 set")
	}
	if rsv3 {
		return noFrame, c.handleProtocolError("RSV3 set")
	}

	switch frameType {
	case CloseMessage, PingMessage, PongMessage:
		if c.readRemaining > maxControlFramePayloadSize {
			return noFrame, c.handleProtocolError("len > 125 for control")
		}
		if !final {
			return noFrame, c.handleProtocolError("FIN not set on control")
		}
	case TextMessage, BinaryMessage:
		if !c.readFinal {
			return noFrame, c.handleProtocolError("data before FIN")
		}
		c.readFinal = final
	case continuationFrame:
		if c.readFinal {
			return noFrame, c.handleProtocolError("continuation after FIN")
		}
		c.readFinal = final
	default:
		return noFrame, c.handleProtocolError("bad opcode " + strconv.Itoa(frameType))
	}

	if mask != c.isServer {
		return noFrame, c.handleProtocolError("bad MASK")
	}

	switch c.readRemaining {
	case 126:
		p, err := c.read(2)
		if err != nil {
			return noFrame, err
		}
		if err := c.setReadRemaining(int64(binary.BigEndian.Uint16(p))); err != nil {
			return noFrame, err
		}
	case 127:
		p, err := c.read(8)
		if err != nil {
			return noFrame, err
		}
		if err := c.setReadRemaining(int64(binary.BigEndian.Uint64(p))); err != nil {
			return noFrame, err
		}
	}

	if mask {
		c.readMaskPos = 0
		p, err := c.read(len(c.readMaskKey))
		if err != nil {
			return noFrame, err
		}
		copy(c.readMaskKey[:], p)
	}

	if frameType == continuationFrame || frameType == TextMessage || frameType == BinaryMessage {
		c.readLength += c.readRemaining
		if c.readLength < 0 {
			return noFrame, ErrReadLimit
		}
		if c.readLimit > 0 && c.readLength > c.readLimit {
			c.WriteControl(CloseMessage, FormatCloseMessage(CloseMessageTooBig, ""), time.Now().Add(writeWait))
			return noFrame, ErrReadLimit
		}
		return frameType, nil
	}

	var payload []byte
	if c.readRemaining > 0 {
		payload, err = c.read(int(c.readRemaining))
		c.setReadRemaining(0)
		if err != nil {
			return noFrame, err
		}
		if c.isServer {
			maskBytes(c.readMaskKey, 0, payload)
		}
	}

	switch frameType {
	case PongMessage:
		if err := c.handlePong(string(payload)); err != nil {
			return noFrame, err
		}
	case PingMessage:
		if err := c.handlePing(string(payload)); err != nil {
			return noFrame, err
		}
	case CloseMessage:
		closeCode := CloseNoStatusReceived
		closeText := ""
		if len(payload) >= 2 {
			closeCode = int(binary.BigEndian.Uint16(payload))
			if !isValidReceivedCloseCode(closeCode) {
				return noFrame, c.handleProtocolError("bad close code " + strconv.Itoa(closeCode))
			}
			closeText = string(payload[2:])
			if !utf8.ValidString(closeText) {
				return noFrame, c.handleProtocolError("invalid utf8 payload in close frame")
			}
		}
		if err := c.handleClose(closeCode, closeText); err != nil {
			return noFrame, err
		}
		return noFrame, &CloseError{Code: closeCode, Text: closeText}
	}
	return frameType, nil
}

func (c *Conn) handleProtocolError(message string) error {
	data := FormatCloseMessage(CloseProtocolError, message)
	if len(data) > maxControlFramePayloadSize {
		data = data[:maxControlFramePayloadSize]
	}
	c.WriteControl(CloseMessage, data, time.Now().Add(writeWait))
	return errors.New("websocket: " + message)
}

// Helper types
type messageWriter struct {
	c         *Conn
	compress  bool
	pos       int
	frameType int
	err       error
}

func (w *messageWriter) endMessage(err error) error {
	if w.err != nil {
		return err
	}
	c := w.c
	w.err = err
	c.writer = nil
	if c.writePool != nil {
		c.writePool.Put(writePoolData{buf: c.writeBuf})
		c.writeBuf = nil
	}
	return err
}

func (w *messageWriter) flushFrame(final bool, extra []byte) error {
	c := w.c
	length := w.pos - maxFrameHeaderSize + len(extra)

	if isControl(w.frameType) && (!final || length > maxControlFramePayloadSize) {
		return w.endMessage(errInvalidControlFrame)
	}

	b0 := byte(w.frameType)
	if final {
		b0 |= finalBit
	}
	if w.compress {
		b0 |= rsv1Bit
	}
	w.compress = false

	b1 := byte(0)
	if !c.isServer {
		b1 |= maskBit
	}

	framePos := 0
	if c.isServer {
		framePos = 4
	}

	switch {
	case length >= 65536:
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | 127
		binary.BigEndian.PutUint64(c.writeBuf[framePos+2:], uint64(length))
	case length > 125:
		framePos += 6
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | 126
		binary.BigEndian.PutUint16(c.writeBuf[framePos+2:], uint16(length))
	default:
		framePos += 8
		c.writeBuf[framePos] = b0
		c.writeBuf[framePos+1] = b1 | byte(length)
	}

	if !c.isServer {
		key := newMaskKey()
		copy(c.writeBuf[maxFrameHeaderSize-4:], key[:])
		maskBytes(key, 0, c.writeBuf[maxFrameHeaderSize:w.pos])
		if len(extra) > 0 {
			return w.endMessage(c.writeFatal(errors.New("websocket: internal error, extra used in client mode")))
		}
	}

	if c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = true

	err := c.write(w.frameType, c.writeDeadline, c.writeBuf[framePos:w.pos], extra)

	if !c.isWriting {
		panic("concurrent write to websocket connection")
	}
	c.isWriting = false

	if err != nil {
		return w.endMessage(err)
	}

	if final {
		w.endMessage(errWriteClosed)
		return nil
	}

	w.pos = maxFrameHeaderSize
	w.frameType = continuationFrame
	return nil
}

func (w *messageWriter) ncopy(max int) (int, error) {
	n := len(w.c.writeBuf) - w.pos
	if n <= 0 {
		if err := w.flushFrame(false, nil); err != nil {
			return 0, err
		}
		n = len(w.c.writeBuf) - w.pos
	}
	if n > max {
		n = max
	}
	return n, nil
}

func (w *messageWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	if len(p) > 2*len(w.c.writeBuf) && w.c.isServer {
		err := w.flushFrame(false, p)
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}

	nn := len(p)
	for len(p) > 0 {
		n, err := w.ncopy(len(p))
		if err != nil {
			return 0, err
		}
		copy(w.c.writeBuf[w.pos:], p[:n])
		w.pos += n
		p = p[n:]
	}
	return nn, nil
}

func (w *messageWriter) WriteString(p string) (int, error) {
	return w.Write([]byte(p))
}

func (w *messageWriter) ReadFrom(r io.Reader) (nn int64, err error) {
	if w.err != nil {
		return 0, w.err
	}
	for {
		if w.pos == len(w.c.writeBuf) {
			err = w.flushFrame(false, nil)
			if err != nil {
				break
			}
		}
		var n int
		n, err = r.Read(w.c.writeBuf[w.pos:])
		w.pos += n
		nn += int64(n)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
	}
	return nn, err
}

func (w *messageWriter) Close() error {
	if w.err != nil {
		return w.err
	}
	return w.flushFrame(true, nil)
}

type messageReader struct{ c *Conn }

func (r *messageReader) Read(b []byte) (int, error) {
	c := r.c
	if c.messageReader != r {
		return 0, io.EOF
	}

	for c.readErr == nil {
		if c.readRemaining > 0 {
			if int64(len(b)) > c.readRemaining {
				b = b[:c.readRemaining]
			}
			n, err := c.br.Read(b)
			c.readErr = hideTempErr(err)
			if c.isServer {
				c.readMaskPos = maskBytes(c.readMaskKey, c.readMaskPos, b[:n])
			}
			rem := c.readRemaining
			rem -= int64(n)
			c.setReadRemaining(rem)
			if c.readRemaining > 0 && c.readErr == io.EOF {
				c.readErr = errUnexpectedEOF
			}
			return n, c.readErr
		}

		if c.readFinal {
			c.messageReader = nil
			return 0, io.EOF
		}

		frameType, err := c.advanceFrame()
		switch {
		case err != nil:
			c.readErr = hideTempErr(err)
		case frameType == TextMessage || frameType == BinaryMessage:
			c.readErr = errors.New("websocket: internal error, unexpected text or binary in Reader")
		}
	}

	err := c.readErr
	if err == io.EOF && c.messageReader == r {
		err = errUnexpectedEOF
	}
	return 0, err
}

func (r *messageReader) Close() error { return nil }

// Utility functions
func newMaskKey() [4]byte {
	n := rand.Uint32()
	return [4]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
}

func hideTempErr(err error) error {
	if e, ok := err.(net.Error); ok && e.Temporary() {
		err = &netError{msg: e.Error(), timeout: e.Timeout()}
	}
	return err
}

func isControl(frameType int) bool {
	return frameType == CloseMessage || frameType == PingMessage || frameType == PongMessage
}

func isData(frameType int) bool {
	return frameType == TextMessage || frameType == BinaryMessage
}

func isValidReceivedCloseCode(code int) bool {
	return validReceivedCloseCodes[code] || (code >= 3000 && code <= 4999)
}

var validReceivedCloseCodes = map[int]bool{
	CloseNormalClosure:      true,
	CloseGoingAway:          true,
	CloseProtocolError:      true,
	CloseUnsupportedData:    true,
	CloseInvalidFramePayloadData: true,
	ClosePolicyViolation:    true,
	CloseMessageTooBig:      true,
	CloseMandatoryExtension: true,
	CloseInternalServerErr:  true,
	CloseServiceRestart:     true,
	CloseTryAgainLater:      true,
}

// FormatCloseMessage formats close code and text
func FormatCloseMessage(closeCode int, text string) []byte {
	if closeCode == CloseNoStatusReceived {
		return []byte{}
	}
	buf := make([]byte, 2+len(text))
	binary.BigEndian.PutUint16(buf, uint16(closeCode))
	copy(buf[2:], text)
	return buf
}