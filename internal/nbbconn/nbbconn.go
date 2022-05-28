// Package nbbconn implements a non-blocking, buffered net.Conn wrapper.
package nbbconn

import (
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/internal/iobufpool"
)

var errClosed = errors.New("closed")
var errWouldBlock = errors.New("would block")

const fakeNonblockingWaitDuration = 100 * time.Millisecond

// Conn is a non-blocking, buffered net.Conn wrapper. It implements net.Conn.
//
// It is designed to solve two problems.
//
// The first is resolving the deadlock that can occur when both sides of a connection are blocked writing because all
// buffers between are full. See https://github.com/jackc/pgconn/issues/27 for discussion.
//
// The second is the inability to use a write deadline with a TLS.Conn without killing the connection.
type Conn struct {
	netConn net.Conn

	readQueue  bufferQueue
	writeQueue bufferQueue

	readFlushLock sync.Mutex

	readDeadlineLock sync.Mutex
	readDeadline     time.Time

	writeDeadlineLock sync.Mutex
	writeDeadline     time.Time

	// Only access with atomics
	closed int64 // 0 = not closed, 1 = closed
}

func New(conn net.Conn) *Conn {
	return &Conn{
		netConn: conn,
	}
}

func (c *Conn) Read(b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, errClosed
	}

	c.readFlushLock.Lock()
	defer c.readFlushLock.Unlock()

	err = c.flush()
	if err != nil {
		return 0, err
	}

	buf := c.readQueue.popFront()
	if buf != nil {
		n = copy(b, buf)
		if n < len(buf) {
			buf = buf[n:]
			c.readQueue.pushFront(buf)
		} else {
			releaseBuf(buf)
		}
		return n, nil
	}

	return c.netConn.Read(b)
}

func (c *Conn) Write(b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, errClosed
	}

	buf := iobufpool.Get(len(b))
	buf = buf[:len(b)]
	copy(buf, b)
	c.writeQueue.pushBack(buf)
	return len(b), nil
}

func (c *Conn) Close() (err error) {
	swapped := atomic.CompareAndSwapInt64(&c.closed, 0, 1)
	if !swapped {
		return errClosed
	}

	defer func() {
		closeErr := c.netConn.Close()
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

func (c *Conn) LocalAddr() net.Addr {
	return c.netConn.LocalAddr()
}

func (c *Conn) RemoteAddr() net.Addr {
	return c.netConn.RemoteAddr()
}

func (c *Conn) SetDeadline(t time.Time) error {
	if c.isClosed() {
		return errClosed
	}

	c.readDeadlineLock.Lock()
	defer c.readDeadlineLock.Unlock()
	c.readDeadline = t

	c.writeDeadlineLock.Lock()
	defer c.writeDeadlineLock.Unlock()
	c.writeDeadline = t

	return c.netConn.SetDeadline(t)
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.isClosed() {
		return errClosed
	}

	c.readDeadlineLock.Lock()
	defer c.readDeadlineLock.Unlock()
	c.readDeadline = t

	return c.netConn.SetReadDeadline(t)
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.isClosed() {
		return errClosed
	}

	c.writeDeadlineLock.Lock()
	defer c.writeDeadlineLock.Unlock()
	c.writeDeadline = t

	return c.netConn.SetWriteDeadline(t)
}

func (c *Conn) Flush() error {
	if c.isClosed() {
		return errClosed
	}

	c.readFlushLock.Lock()
	defer c.readFlushLock.Unlock()
	return c.flush()
}

// flush does the actual work of flushing the writeQueue. readFlushLock must already be held.
func (c *Conn) flush() error {
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
				if !errors.Is(err, errWouldBlock) {
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

func (c *Conn) bufferNonblockingRead() (stopChan chan struct{}, errChan chan error) {
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
				if !errors.Is(err, errWouldBlock) {
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

func (c *Conn) isClosed() bool {
	closed := atomic.LoadInt64(&c.closed)
	return closed == 1
}

func (c *Conn) nonblockingWrite(b []byte) (n int, err error) {
	return c.fakeNonblockingWrite(b)
}

func (c *Conn) fakeNonblockingWrite(b []byte) (n int, err error) {
	c.writeDeadlineLock.Lock()
	defer c.writeDeadlineLock.Unlock()

	deadline := time.Now().Add(fakeNonblockingWaitDuration)
	if c.writeDeadline.IsZero() || deadline.Before(c.writeDeadline) {
		err = c.netConn.SetWriteDeadline(deadline)
		if err != nil {
			return 0, err
		}
		defer func() {
			// Ignoring error resetting deadline as there is nothing that can reasonably be done if it fails.
			c.netConn.SetWriteDeadline(c.writeDeadline)

			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = errWouldBlock
				}
			}
		}()
	}

	return c.netConn.Write(b)
}

func (c *Conn) nonblockingRead(b []byte) (n int, err error) {
	return c.fakeNonblockingRead(b)
}

func (c *Conn) fakeNonblockingRead(b []byte) (n int, err error) {
	c.readDeadlineLock.Lock()
	defer c.readDeadlineLock.Unlock()

	deadline := time.Now().Add(fakeNonblockingWaitDuration)
	if c.readDeadline.IsZero() || deadline.Before(c.readDeadline) {
		err = c.netConn.SetReadDeadline(deadline)
		if err != nil {
			return 0, err
		}
		defer func() {
			// Ignoring error resetting deadline as there is nothing that can reasonably be done if it fails.
			c.netConn.SetReadDeadline(c.readDeadline)

			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					err = errWouldBlock
				}
			}
		}()
	}

	return c.netConn.Read(b)
}

// syscall.Conn is interface
