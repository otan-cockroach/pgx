// Package nbconn implements a non-blocking net.Conn wrapper.
//
// It is designed to solve three problems.
//
// The first is resolving the deadlock that can occur when both sides of a connection are blocked writing because all
// buffers between are full. See https://github.com/jackc/pgconn/issues/27 for discussion.
//
// The second is the inability to use a write deadline with a TLS.Conn without killing the connection.
//
// The third is to efficiently check if a connection has been closed via a non-blocking read.
package nbconn

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/internal/iobufpool"
)

var errClosed = errors.New("closed")
var ErrWouldBlock = new(wouldBlockError)

const fakeNonblockingWaitDuration = 100 * time.Millisecond

// NonBlockingDeadline is a magic value that when passed to Set[Read]Deadline places the connection in non-blocking read
// mode.
var NonBlockingDeadline = time.Date(1900, 1, 1, 0, 0, 0, 608536336, time.UTC)

// disableSetDeadlineDeadline is a magic value that when passed to Set[Read|Write]Deadline causes those methods to
// ignore all future calls.
var disableSetDeadlineDeadline = time.Date(1900, 1, 1, 0, 0, 0, 968549727, time.UTC)

// wouldBlockError implements net.Error so tls.Conn will recognize ErrWouldBlock as a temporary error.
type wouldBlockError struct{}

func (*wouldBlockError) Error() string {
	return "would block"
}

func (*wouldBlockError) Timeout() bool   { return true }
func (*wouldBlockError) Temporary() bool { return true }

// Conn is a net.Conn where Write never blocks and always succeeds. Flush must be called to actually write to the
// underlying connection.
type Conn interface {
	net.Conn
	Flush() error
}

// NetConn is a non-blocking net.Conn wrapper. It implements net.Conn.
type NetConn struct {
	conn net.Conn

	readQueue  bufferQueue
	writeQueue bufferQueue

	readFlushLock sync.Mutex

	readDeadlineLock sync.Mutex
	readDeadline     time.Time
	readNonblocking  bool

	writeDeadlineLock sync.Mutex
	writeDeadline     time.Time

	// Only access with atomics
	closed int64 // 0 = not closed, 1 = closed
}

func NewNetConn(conn net.Conn) *NetConn {
	return &NetConn{
		conn: conn,
	}
}

// Read implements io.Reader.
func (c *NetConn) Read(b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, errClosed
	}

	c.readFlushLock.Lock()
	defer c.readFlushLock.Unlock()

	err = c.flush()
	if err != nil {
		return 0, err
	}

	for n < len(b) {
		buf := c.readQueue.popFront()
		if buf == nil {
			break
		}
		copiedN := copy(b[n:], buf)
		if copiedN < len(buf) {
			buf = buf[copiedN:]
			c.readQueue.pushFront(buf)
		} else {
			releaseBuf(buf)
		}
		n += copiedN
	}

	if n == len(b) {
		return n, nil
	}

	var readNonblocking bool
	c.readDeadlineLock.Lock()
	readNonblocking = c.readNonblocking
	c.readDeadlineLock.Unlock()

	var readN int
	if readNonblocking {
		readN, err = c.nonblockingRead(b[n:])
	} else {
		readN, err = c.conn.Read(b[n:])
	}
	n += readN
	return n, err
}

// Write implements io.Writer. It never blocks due to buffering all writes. It will only return an error if the Conn is
// closed. Call Flush to actually write to the underlying connection.
func (c *NetConn) Write(b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, errClosed
	}

	buf := iobufpool.Get(len(b))
	copy(buf, b)
	c.writeQueue.pushBack(buf)
	return len(b), nil
}

func (c *NetConn) Close() (err error) {
	swapped := atomic.CompareAndSwapInt64(&c.closed, 0, 1)
	if !swapped {
		return errClosed
	}

	defer func() {
		closeErr := c.conn.Close()
		if err == nil {
			err = closeErr
		}
	}()

	c.readFlushLock.Lock()
	defer c.readFlushLock.Unlock()
	err = c.flush()
	if err != nil {
		return err
	}

	return nil
}

func (c *NetConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *NetConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline is the equivalent of calling SetReadDealine(t) and SetWriteDeadline(t).
func (c *NetConn) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets the read deadline as t. If t == NonBlockingDeadline then future reads will be non-blocking.
func (c *NetConn) SetReadDeadline(t time.Time) error {
	if c.isClosed() {
		return errClosed
	}

	c.readDeadlineLock.Lock()
	defer c.readDeadlineLock.Unlock()
	if c.readDeadline == disableSetDeadlineDeadline {
		return nil
	}
	if t == disableSetDeadlineDeadline {
		c.readDeadline = t
		return nil
	}

	if t == NonBlockingDeadline {
		c.readNonblocking = true
		t = time.Time{}
	} else {
		c.readNonblocking = false
	}

	c.readDeadline = t

	return c.conn.SetReadDeadline(t)
}

func (c *NetConn) SetWriteDeadline(t time.Time) error {
	if c.isClosed() {
		return errClosed
	}

	c.writeDeadlineLock.Lock()
	defer c.writeDeadlineLock.Unlock()
	if c.writeDeadline == disableSetDeadlineDeadline {
		return nil
	}
	if t == disableSetDeadlineDeadline {
		c.writeDeadline = t
		return nil
	}

	c.writeDeadline = t

	return c.conn.SetWriteDeadline(t)
}

func (c *NetConn) Flush() error {
	if c.isClosed() {
		return errClosed
	}

	c.readFlushLock.Lock()
	defer c.readFlushLock.Unlock()
	return c.flush()
}

// flush does the actual work of flushing the writeQueue. readFlushLock must already be held.
func (c *NetConn) flush() error {
	var stopChan chan struct{}
	var errChan chan error

	defer func() {
		if stopChan != nil {
			select {
			case stopChan <- struct{}{}:
			case <-errChan:
			}
		}
	}()

	for buf := c.writeQueue.popFront(); buf != nil; buf = c.writeQueue.popFront() {
		remainingBuf := buf
		for len(remainingBuf) > 0 {
			n, err := c.nonblockingWrite(remainingBuf)
			remainingBuf = remainingBuf[n:]
			if err != nil {
				if !errors.Is(err, ErrWouldBlock) {
					buf = buf[:len(remainingBuf)]
					copy(buf, remainingBuf)
					c.writeQueue.pushFront(buf)
					return err
				}

				// Writing was blocked. Reading might unblock it.
				if stopChan == nil {
					stopChan, errChan = c.bufferNonblockingRead()
				}

				select {
				case err := <-errChan:
					stopChan = nil
					return err
				default:
				}

			}
		}
		releaseBuf(buf)
	}

	return nil
}

func (c *NetConn) bufferNonblockingRead() (stopChan chan struct{}, errChan chan error) {
	stopChan = make(chan struct{})
	errChan = make(chan error, 1)

	go func() {
		for {
			buf := iobufpool.Get(8 * 1024)
			n, err := c.nonblockingRead(buf)
			if n > 0 {
				buf = buf[:n]
				c.readQueue.pushBack(buf)
			}

			if err != nil {
				if !errors.Is(err, ErrWouldBlock) {
					errChan <- err
					return
				}
			}

			select {
			case <-stopChan:
				return
			default:
			}
		}
	}()

	return stopChan, errChan
}

func (c *NetConn) isClosed() bool {
	closed := atomic.LoadInt64(&c.closed)
	return closed == 1
}

func (c *NetConn) nonblockingWrite(b []byte) (n int, err error) {
	return c.fakeNonblockingWrite(b)
}

func (c *NetConn) fakeNonblockingWrite(b []byte) (n int, err error) {
	c.writeDeadlineLock.Lock()
	defer c.writeDeadlineLock.Unlock()

	deadline := time.Now().Add(fakeNonblockingWaitDuration)
	if c.writeDeadline.IsZero() || deadline.Before(c.writeDeadline) {
		err = c.conn.SetWriteDeadline(deadline)
		if err != nil {
			return 0, err
		}
		defer func() {
			// Ignoring error resetting deadline as there is nothing that can reasonably be done if it fails.
			c.conn.SetWriteDeadline(c.writeDeadline)

			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = ErrWouldBlock
				}
			}
		}()
	}

	return c.conn.Write(b)
}

func (c *NetConn) nonblockingRead(b []byte) (n int, err error) {
	return c.fakeNonblockingRead(b)
}

func (c *NetConn) fakeNonblockingRead(b []byte) (n int, err error) {
	c.readDeadlineLock.Lock()
	defer c.readDeadlineLock.Unlock()

	deadline := time.Now().Add(fakeNonblockingWaitDuration)
	if c.readDeadline.IsZero() || deadline.Before(c.readDeadline) {
		err = c.conn.SetReadDeadline(deadline)
		if err != nil {
			return 0, err
		}
		defer func() {
			// Ignoring error resetting deadline as there is nothing that can reasonably be done if it fails.
			c.conn.SetReadDeadline(c.readDeadline)

			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = ErrWouldBlock
				}
			}
		}()
	}

	return c.conn.Read(b)
}

// syscall.Conn is interface

// TLSClient establishes a TLS connection as a client over conn using config.
//
// To avoid the first Read on the returned *TLSConn also triggering a Write due to the TLS handshake and thereby
// potentially causing a read and write deadlines to behave unexpectedly, Handshake is called explicitly before the
// *TLSConn is returned.
func TLSClient(conn *NetConn, config *tls.Config) (*TLSConn, error) {
	tc := tls.Client(conn, config)
	err := tc.Handshake()
	if err != nil {
		return nil, err
	}

	// Ensure last written part of Handshake is actually sent.
	err = conn.Flush()
	if err != nil {
		return nil, err
	}

	return &TLSConn{
		tlsConn: tc,
		nbConn:  conn,
	}, nil
}

// TLSConn is a TLS wrapper around a *Conn. It works around a temporary write error (such as a timeout) being fatal to a
// tls.Conn.
type TLSConn struct {
	tlsConn *tls.Conn
	nbConn  *NetConn
}

func (tc *TLSConn) Read(b []byte) (n int, err error)  { return tc.tlsConn.Read(b) }
func (tc *TLSConn) Write(b []byte) (n int, err error) { return tc.tlsConn.Write(b) }
func (tc *TLSConn) Flush() error                      { return tc.nbConn.Flush() }
func (tc *TLSConn) LocalAddr() net.Addr               { return tc.tlsConn.LocalAddr() }
func (tc *TLSConn) RemoteAddr() net.Addr              { return tc.tlsConn.RemoteAddr() }

func (tc *TLSConn) Close() error {
	// tls.Conn.closeNotify() sets a 5 second deadline to avoid blocking, sends a TLS alert close notification, and then
	// sets the deadline to now. This causes NetConn's Close not to be able to flush the write buffer. Instead we set our
	// own 5 second deadline then make all set deadlines no-op.
	tc.tlsConn.SetDeadline(time.Now().Add(time.Second * 5))
	tc.tlsConn.SetDeadline(disableSetDeadlineDeadline)

	return tc.tlsConn.Close()
}

func (tc *TLSConn) SetDeadline(t time.Time) error      { return tc.tlsConn.SetDeadline(t) }
func (tc *TLSConn) SetReadDeadline(t time.Time) error  { return tc.tlsConn.SetReadDeadline(t) }
func (tc *TLSConn) SetWriteDeadline(t time.Time) error { return tc.tlsConn.SetWriteDeadline(t) }
