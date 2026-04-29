package tcpclient

import (
	"context"
	"net"
	"sync"
	"time"
)

type Conn struct {
	conn         net.Conn
	writeTimeout time.Duration
	sendMu       sync.Mutex
}

func Dial(ctx context.Context, address string, writeTimeout time.Duration) (*Conn, error) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return &Conn{conn: conn, writeTimeout: writeTimeout}, nil
}

func (c *Conn) Send(msg Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	return WriteMessage(c.conn, msg)
}

func (c *Conn) Receive() (Message, error) {
	return ReadMessage(c.conn)
}

func (c *Conn) Close() error {
	return c.conn.Close()
}
