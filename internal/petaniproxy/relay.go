package petaniproxy

import (
	"io"
	"net"
	"sync"
)

// drainConn wraps a net.Conn so reads first drain r — bytes already buffered
// from the connection (e.g. by a bufio.Reader used to parse the CONNECT
// response) — before reading live from the wire.
type drainConn struct {
	net.Conn
	r io.Reader
}

func (d *drainConn) Read(b []byte) (int, error) { return d.r.Read(b) }

// relayConns shuttles bytes bidirectionally between a and b until either side
// hits EOF or an error, then closes both. Mirrors server.py's pipe_sockets
// (which used select with a 30s idle timeout; here the relay ends on EOF,
// so long-lived tunnels are not cut off).
func relayConns(a, b net.Conn) {
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = a.Close()
			_ = b.Close()
			close(done)
		})
	}
	go func() { _, _ = io.Copy(a, b); stop() }()
	go func() { _, _ = io.Copy(b, a); stop() }()
	<-done
}
