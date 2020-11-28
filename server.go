package socks6

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

var (
	ErrReadTruncatedBuffer = fmt.Errorf("read truncated buffer")
	ErrCopyEmptyBuffer     = fmt.Errorf("copy empty buffer")
	ErrWrongSocksVersion   = fmt.Errorf("wrong socks version")
	ErrClosedListener      = fmt.Errorf("closed listener")
	ErrWrongSocksCommand   = fmt.Errorf("wrong socks command")
	ErrLoopbackNotAllowed  = fmt.Errorf("loopback was not allowed")
)

const (
	socksVer4        = 4
	success          = 90
	errRejected      = 91
	errFailedConnect = 92
	errDiffIds       = 93
)

// Server is socks 6 server
type Server struct {
	dialTimeout time.Duration
	sleepTime   time.Duration
	ln          net.Listener
	od          sync.Once
	ch          chan struct{}
	conns       map[net.Conn]net.Conn
}

func NewSocksServer(dialTimeout, sleepTime time.Duration, connsSize int) Server {
	return Server{
		dialTimeout: dialTimeout,
		sleepTime:   sleepTime,
		od:          sync.Once{},
		ch:          make(chan struct{}),
		conns:       make(map[net.Conn]net.Conn, connsSize),
	}
}

func (srv *Server) Closed() bool {
	select {
	case <-srv.ch:
		return true
	default:
		return false
	}
}

func (srv *Server) netCopy(input, output net.Conn) (err error) {
	var count int64
	count, err = io.Copy(output, input)
	if err == nil && count < 0 {
		err = ErrCopyEmptyBuffer
		return
	}
	return
}

// handshake
func (srv *Server) handshake(conn net.Conn) (err error) {
	var n int
	vn := make([]byte, 1)
	n, err = conn.Read(vn)
	if err != nil {
		return
	}
	if n != 1 {
		err = ErrReadTruncatedBuffer
		return
	}
	switch vn[0] {
	case 4:
		err = srv.handshake4(conn)
		if err != nil {
			conn.Write([]byte{vn[0], errRejected})
			return
		}
		return
	default:
		err = ErrWrongSocksVersion
	}
	return
}

// handshake4 do the socks4 handshake
// only support connect command
// and the data should be: VN, CD, PORT, IP
// spec: https://www.openssh.com/txt/socks4.protocol
func (srv *Server) handshake4(conn net.Conn) (err error) {
	var n int
	buf := make([]byte, 8)
	n, err = conn.Read(buf)
	if err != nil {
		return
	}
	if n != 8 {
		return ErrReadTruncatedBuffer
	}
	cmd := buf[0]
	port := binary.BigEndian.Uint16(buf[1:3])
	ip := net.IP(buf[3:7])
	if cmd != 1 {
		err = ErrWrongSocksVersion
		return
	}
	if ip.IsLoopback() {
		err = ErrLoopbackNotAllowed
		return
	}
	host := net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
	sconn, err := net.Dial("tcp", host)
	if err != nil {
		return
	}
	srv.conns[conn] = sconn
	go func(conn, sconn net.Conn) {
		go srv.netCopy(sconn, conn)
		srv.netCopy(conn, sconn)
		sconn.Close()
		conn.Close()
		delete(srv.conns, conn)
	}(conn, sconn)
	// write socks result
	buf[0] = 0
	buf[1] = success
	conn.Write(buf)
	return
}

// Serve net.Listener
func (srv *Server) Serve(ln net.Listener) error {
	if srv.Closed() {
		return fmt.Errorf("socks server is closed")
	}
	srv.ln = ln
	for {
		conn, err := ln.Accept()
		if err != nil {
			if srv.Closed() {
				err = ErrClosedListener
				return err
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				// sleep for a while
				time.Sleep(srv.sleepTime)
				continue
			}
		}
		go func() {
			if err := srv.handshake(conn); err != nil {
				// failed to handshake
			}
		}()
	}
}

// ListenAndServe listen and serve socks server to hostname
// hostname should be [ip]:[port]
func (srv *Server) ListenAndServe(hostname string) error {
	if srv.Closed() {
		return fmt.Errorf("socks server is closed")
	}
	ln, err := net.Listen("tcp", hostname)
	if err != nil {
		return err
	}
	return srv.Serve(ln)
}

// Close socks server
func (srv *Server) Close() {
	srv.od.Do(func() {
		srv.ln.Close()
		close(srv.ch)
		for oc, sc := range srv.conns {
			oc.Close()
			sc.Close()
		}
	})
}
