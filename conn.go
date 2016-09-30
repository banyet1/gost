package gost

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"github.com/ginuerzh/gosocks5"
	"github.com/golang/glog"
	"github.com/shadowsocks/shadowsocks-go/shadowsocks"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

type ProxyConn struct {
	conn           net.Conn
	node           ProxyNode
	handshaked     bool
	handshakeMutex sync.Mutex
	handshakeErr   error
}

func NewProxyConn(conn net.Conn, node ProxyNode) *ProxyConn {
	return &ProxyConn{
		conn: conn,
		node: node,
	}
}

// Handshake based on the proxy node info: transport, protocol, authentication, etc.
//
// NOTE: http2 will be downgrade to http (for protocol) and tls (for transport).
func (c *ProxyConn) Handshake() error {
	c.handshakeMutex.Lock()
	defer c.handshakeMutex.Unlock()

	if err := c.handshakeErr; err != nil {
		return err
	}
	if c.handshaked {
		return nil
	}
	c.handshakeErr = c.handshake()
	return c.handshakeErr
}

func (c *ProxyConn) handshake() error {
	var tlsUsed bool

	switch c.node.Transport {
	case "ws": // websocket connection
		conn, err := wsClient("ws", c.conn, c.node.Addr)
		if err != nil {
			return err
		}
		c.conn = conn
	case "wss": // websocket security
		tlsUsed = true
		conn, err := wsClient("wss", c.conn, c.node.Addr)
		if err != nil {
			return err
		}
		c.conn = conn
	case "tls", "http2": // tls connection
		tlsUsed = true
		cfg := &tls.Config{
			InsecureSkipVerify: c.node.insecureSkipVerify(),
			ServerName:         c.node.serverName,
		}
		c.conn = tls.Client(c.conn, cfg)
	default:
	}

	switch c.node.Protocol {
	case "socks", "socks5": // socks5 handshake with auth and tls supported
		selector := &clientSelector{
			methods: []uint8{
				gosocks5.MethodNoAuth,
				gosocks5.MethodUserPass,
				//MethodTLS,
			},
			user: c.node.User,
		}

		if !tlsUsed { // if transport is not security, enable security socks5
			selector.methods = append(selector.methods, MethodTLS)
		}

		conn := gosocks5.ClientConn(c.conn, selector)
		if err := conn.Handleshake(); err != nil {
			return err
		}
		c.conn = conn
	case "ss": // shadowsocks
		if c.node.User != nil {
			method := c.node.User.Username()
			password, _ := c.node.User.Password()
			cipher, err := shadowsocks.NewCipher(method, password)
			if err != nil {
				return err
			}
			c.conn = shadowsocks.NewConn(c.conn, cipher)
		}
	case "http", "http2":
		fallthrough
	default:
	}

	c.handshaked = true

	return nil
}

// Connect to addr through this proxy node
func (c *ProxyConn) Connect(addr string) error {
	switch c.node.Protocol {
	case "ss": // shadowsocks
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		p, _ := strconv.Atoi(port)
		req := gosocks5.NewRequest(gosocks5.CmdConnect, &gosocks5.Addr{
			Type: gosocks5.AddrDomain,
			Host: host,
			Port: uint16(p),
		})
		buf := bytes.Buffer{}
		if err := req.Write(&buf); err != nil {
			return err
		}
		b := buf.Bytes()
		if _, err := c.Write(b[3:]); err != nil {
			return err
		}

		glog.V(LDEBUG).Infoln(req)
	case "socks", "socks5":
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return err
		}
		p, _ := strconv.Atoi(port)
		req := gosocks5.NewRequest(gosocks5.CmdConnect, &gosocks5.Addr{
			Type: gosocks5.AddrDomain,
			Host: host,
			Port: uint16(p),
		})
		if err := req.Write(c); err != nil {
			return err
		}
		glog.V(LDEBUG).Infoln(req)

		rep, err := gosocks5.ReadReply(c)
		if err != nil {
			return err
		}
		glog.V(LDEBUG).Infoln(rep)
		if rep.Rep != gosocks5.Succeeded {
			return errors.New("Service unavailable")
		}
	case "http", "http2":
		fallthrough
	default:
		req := &http.Request{
			Method:     http.MethodConnect,
			URL:        &url.URL{Host: addr},
			Host:       addr,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     make(http.Header),
		}
		req.Header.Set("Proxy-Connection", "keep-alive")
		if c.node.User != nil {
			req.Header.Set("Proxy-Authorization",
				"Basic "+base64.StdEncoding.EncodeToString([]byte(c.node.User.String())))
		}
		if err := req.Write(c); err != nil {
			return err
		}
		if glog.V(LDEBUG) {
			dump, _ := httputil.DumpRequest(req, false)
			glog.Infoln(string(dump))
		}

		resp, err := http.ReadResponse(bufio.NewReader(c), req)
		if err != nil {
			return err
		}
		if glog.V(LDEBUG) {
			dump, _ := httputil.DumpResponse(resp, false)
			glog.Infoln(string(dump))
		}
		if resp.StatusCode != http.StatusOK {
			return errors.New(resp.Status)
		}
	}

	return nil
}

func (c *ProxyConn) Read(b []byte) (n int, err error) {
	return c.conn.Read(b)
}

func (c *ProxyConn) Write(b []byte) (n int, err error) {
	return c.conn.Write(b)
}

func (c *ProxyConn) Close() error {
	return c.conn.Close()
}

func (c *ProxyConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *ProxyConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *ProxyConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

func (c *ProxyConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *ProxyConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}
