WebSocket - Gorilla Fork with Full Path Support

https://img.shields.io/badge/Go-1.26%2B-blue
https://img.shields.io/github/go-mod/go-version/MakhlukTunnel/websocket

Fork of Gorilla WebSocket with enhanced support for full path/opaque URLs and custom HTTP methods.

📋 Table of Contents

· Overview
· Installation
· Features
· Core Concepts
· Server Usage
· Client Usage
· Enhanced Features
· API Reference
· Examples
· License

Overview

This is a fork of the popular Gorilla WebSocket library with additional support for full path/opaque URLs and custom HTTP methods. Perfect for applications that need to pass complex paths or custom methods through WebSocket connections.

Installation

```bash
go get github.com/MakhlukTunnel/websocket
```

Features

Core Features (from Gorilla)

· ✅ Full WebSocket protocol implementation (RFC 6455)
· ✅ Message compression (RFC 7692)
· ✅ Concurrent write safety
· ✅ Buffer pooling for performance
· ✅ Proxy support (HTTP, SOCKS5)
· ✅ TLS/SSL support

Enhanced Features

· ✅ Full Path/Opaque URL Support - Pass complex paths like /api/v1/users:123
· ✅ Custom HTTP Methods - Use methods like PUBLISH, SUBSCRIBE, DELETE in WebSocket handshake
· ✅ Path Parameter Support - Parse parameters directly from URL path
· ✅ Backward Compatible - Works exactly like original Gorilla

Core Concepts

WebSocket Flow

```
Client                     Server
  |                          |
  |--- HTTP Upgrade Request ->|
  |   (with custom method/    |
  |    full path support)     |
  |                          |
  |<- HTTP 101 Switching ----|
  |   Protocols              |
  |                          |
  |=== Bidirectional Data ===|
  |   Text/Binary Frames     |
  |   Ping/Pong Frames       |
  |   Close Frames           |
  |===========================|
```

Architecture

```
┌─────────────────────────────────────┐
│           WebSocket Conn             │
├─────────────────────────────────────┤
│ - Read/Write Mutex                   │
│ - Compression Handlers               │
│ - Frame Readers/Writers              │
│ - Control Message Handlers           │
└───────────┬─────────────────────────┘
            │
    ┌───────┴───────┐
    ▼               ▼
┌─────────┐   ┌─────────┐
│ Client  │   │ Server  │
│ Dialer  │   │Upgrader │
└─────────┘   └─────────┘
    │               │
    └───────┬───────┘
            ▼
┌─────────────────────────────────────┐
│         Proxy Support                │
│ - HTTP Proxy                         │
│ - SOCKS5 Proxy                        │
│ - Environment Variables               │
└─────────────────────────────────────┘
```

Server Usage

Basic Server

```go
package main

import (
    "log"
    "net/http"
    "github.com/MakhlukTunnel/websocket"
)

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return true // Allow all origins
    },
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
    // Upgrade connection
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Print("Upgrade failed:", err)
        return
    }
    defer conn.Close()

    // Echo messages
    for {
        messageType, message, err := conn.ReadMessage()
        if err != nil {
            break
        }
        err = conn.WriteMessage(messageType, message)
        if err != nil {
            break
        }
    }
}

func main() {
    http.HandleFunc("/ws", echoHandler)
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

Server with Custom Options

```go
upgrader := websocket.Upgrader{
    HandshakeTimeout:  10 * time.Second,
    ReadBufferSize:    1024,
    WriteBufferSize:   1024,
    EnableCompression: true,
    Subprotocols:      []string{"chat", "superchat"},
    CheckOrigin: func(r *http.Request) bool {
        origin := r.Header.Get("Origin")
        return origin == "https://example.com"
    },
}
```

Client Usage

Basic Client

```go
package main

import (
    "log"
    "github.com/MakhlukTunnel/websocket"
)

func main() {
    // Connect to WebSocket server
    conn, _, err := websocket.DefaultDialer.Dial("ws://localhost:8080/ws", nil)
    if err != nil {
        log.Fatal("Dial failed:", err)
    }
    defer conn.Close()

    // Send message
    err = conn.WriteMessage(websocket.TextMessage, []byte("Hello, WebSocket!"))
    if err != nil {
        log.Fatal("Write failed:", err)
    }

    // Read response
    _, message, err := conn.ReadMessage()
    if err != nil {
        log.Fatal("Read failed:", err)
    }
    log.Printf("Received: %s", message)
}
```

Enhanced Features

1. Full Path/Opaque URL Support

Pass complex paths with parameters directly in the URL:

```go
// Connect with full path parameters
conn, _, err := websocket.DefaultDialer.Dial(
    "ws://localhost:8080/api/v1/users:123/messages:456", 
    nil,
)

// On server side, path will be available
// r.URL.Opaque = "api/v1/users:123/messages:456"
```

2. Custom HTTP Methods

Use any HTTP method for WebSocket handshake:

```go
// Custom Dialer with PUBLISH method
dialer := &websocket.Dialer{
    Method: "PUBLISH",
}

conn, _, err := dialer.Dial(
    "ws://localhost:8080/topics/news",
    nil,
)

// Or embed method in URL
conn, _, err := websocket.DefaultDialer.Dial(
    "ws://localhost:8080/PUBLISH:topics:news",
    nil,
)
```

3. Path Parameter Parsing

```go
// URL format: ws://server:port/[METHOD:]path:param1:param2

// Example 1: Just path
// ws://localhost:8080/users/profile

// Example 2: Path with parameters
// ws://localhost:8080/users:123:active

// Example 3: Custom method with parameters  
// ws://localhost:8080/PUBLISH:topics:news:sports
```

API Reference

Constants

```go
// Message Types
const (
    TextMessage   = 1
    BinaryMessage = 2
    CloseMessage  = 8
    PingMessage   = 9
    PongMessage   = 10
)

// Close Codes
const (
    CloseNormalClosure           = 1000
    CloseGoingAway               = 1001
    CloseProtocolError           = 1002
    CloseUnsupportedData         = 1003
    CloseNoStatusReceived        = 1005
    CloseAbnormalClosure         = 1006
    // ... see documentation for full list
)
```

Dialer Options

```go
type Dialer struct {
    // Custom HTTP method (default: "GET")
    Method string
    
    // Network dial functions
    NetDial            func(network, addr string) (net.Conn, error)
    NetDialContext     func(ctx context.Context, network, addr string) (net.Conn, error)
    NetDialTLSContext  func(ctx context.Context, network, addr string) (net.Conn, error)
    
    // Proxy configuration
    Proxy func(*http.Request) (*url.URL, error)
    
    // TLS configuration
    TLSClientConfig *tls.Config
    
    // Timeouts and buffers
    HandshakeTimeout time.Duration
    ReadBufferSize   int
    WriteBufferSize  int
    
    // Features
    Subprotocols      []string
    EnableCompression bool
    Jar              http.CookieJar
}
```

Upgrader Options

```go
type Upgrader struct {
    HandshakeTimeout  time.Duration
    ReadBufferSize    int
    WriteBufferSize   int
    WriteBufferPool   BufferPool
    Subprotocols      []string
    EnableCompression bool
    CheckOrigin       func(r *http.Request) bool
    Error             func(w http.ResponseWriter, r *http.Request, status int, reason error)
}
```

Conn Methods

```go
// Reading
NextReader() (int, io.Reader, error)
ReadMessage() (int, []byte, error)
ReadJSON(v interface{}) error

// Writing
NextWriter(messageType int) (io.WriteCloser, error)
WriteMessage(messageType int, data []byte) error
WriteJSON(v interface{}) error
WriteControl(messageType int, data []byte, deadline time.Time) error
WritePreparedMessage(pm *PreparedMessage) error

// Control
Close() error
SetReadLimit(limit int64)
SetReadDeadline(t time.Time) error
SetWriteDeadline(t time.Time) error
SetCompressionLevel(level int) error
EnableWriteCompression(enable bool)

// Handlers
SetPingHandler(h func(appData string) error)
SetPongHandler(h func(appData string) error)
SetCloseHandler(h func(code int, text string) error)

// Info
Subprotocol() string
LocalAddr() net.Addr
RemoteAddr() net.Addr
UnderlyingConn() net.Conn
```

Examples

Chat Application

```go
// Server
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}

var clients = make(map[*websocket.Conn]bool)
var broadcast = make(chan []byte)

func handleConnections(w http.ResponseWriter, r *http.Request) {
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Fatal(err)
    }
    defer conn.Close()
    
    clients[conn] = true
    
    for {
        _, msg, err := conn.ReadMessage()
        if err != nil {
            delete(clients, conn)
            break
        }
        broadcast <- msg
    }
}

func handleMessages() {
    for {
        msg := <-broadcast
        for client := range clients {
            err := client.WriteMessage(websocket.TextMessage, msg)
            if err != nil {
                client.Close()
                delete(clients, client)
            }
        }
    }
}
```

Custom Method Client

```go
// Subscribe to topic with custom method
dialer := &websocket.Dialer{
    Method: "SUBSCRIBE",
}

conn, _, err := dialer.Dial(
    "ws://message-broker:8080/topics/news",
    http.Header{"Authorization": []string{"Bearer token123"}},
)

// Send subscription message
conn.WriteMessage(websocket.TextMessage, []byte(`{"topic":"news","filter":"sports"}`))
```

Full Path Parameters

```go
// Server handler that parses path parameters
func userHandler(w http.ResponseWriter, r *http.Request) {
    // Parse custom path
    // URL: ws://localhost:8080/users:123:active
    path := strings.TrimPrefix(r.URL.Path, "/")
    parts := strings.Split(path, ":")
    
    if len(parts) >= 2 {
        userID := parts[1]
        status := parts[2]
        
        log.Printf("User %s is %s", userID, status)
    }
    
    conn, err := upgrader.Upgrade(w, r, nil)
    if err != nil {
        log.Print(err)
        return
    }
    defer conn.Close()
    
    // Handle WebSocket connection...
}
```

Performance Tips

1. Use Buffer Pools for high-load applications
2. Enable Compression for text-heavy messages
3. Set Read Limits to prevent memory exhaustion
4. Use Prepared Messages for repeated broadcasts
5. Handle Ping/Pong to detect dead connections

```go
// Example: Optimized for high load
upgrader := websocket.Upgrader{
    WriteBufferPool: &sync.Pool{
        New: func() interface{} {
            return make([]byte, 4096)
        },
    },
    EnableCompression: true,
}

conn.SetReadLimit(65536) // 64KB max message size
```

License

This project is a fork of Gorilla WebSocket and maintains the same BSD-style license.

---

Note: This fork maintains 100% backward compatibility with original Gorilla WebSocket while adding support for full path/opaque URLs and custom HTTP methods.