package gosmpp

import (
	"crypto/tls"
	"fmt"
	"github.com/linxGnu/gosmpp/data"
	"github.com/linxGnu/gosmpp/pdu"
	"net"
)

type Server struct {
	Addr    string
	TLS     *tls.Config
	Handler func(p pdu.PDU) (res pdu.PDU, err error)
	Auth    *Auth
}

func (s *Server) Start() error {
	fmt.Println("starting server on", s.Addr)
	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	for {
		var c net.Conn
		c, err = l.Accept()
		if err != nil {
			return err
		}
		go handleConn(*s, c)
	}

}

func handleConn(s Server, conn net.Conn) {
	fmt.Println("incoming connection", conn.RemoteAddr().String())
	c := NewConnection(conn)
	sc := &serverConn{
		Connection: c,
		s:          s,
	}
	defer sc.Close()
	err := sc.start()
	if err != nil {
		fmt.Println("connection error", err)
	}
}

type serverConn struct {
	*Connection
	s     Server
	bound bool
}

func (c *serverConn) start() error {
	fmt.Println("starting connection")
	if c.s.Auth != nil {
		err := bindWithAuth(c, *c.s.Auth)
		if err != nil {
			return err
		}
	}
	for {
		p, err := pdu.Parse(c)
		if err != nil {
			return err
		}
		defr := handleDefault(p)
		if defr != nil {
			_, err = c.WritePDU(*defr)

			if err != nil {
				return err
			}
			continue
		}

		res, err := c.s.Handler(p)
		if err != nil {
			return err
		}

		_, err = c.WritePDU(res)

		if err != nil {
			return err
		}
	}
}

func handleDefault(p pdu.PDU) (res *pdu.PDU) {
	switch pd := p.(type) {
	case *pdu.EnquireLink:
		{
			r := pd.GetResponse()
			return &r
		}
	case *pdu.Unbind:
		{
			r := pd.GetResponse()
			return &r
		}
	}
	return
}

func bindWithAuth(c *serverConn, auth Auth) error {
	fmt.Println("binding connection with auth")
	var (
		p   pdu.PDU
		err error
	)
	if p, err = pdu.Parse(c); err != nil {
		return err
	}

	if req, ok := p.(*pdu.BindRequest); ok {
		res := pdu.NewBindResp(*req)
		if req.SystemID == auth.SystemID && req.Password == auth.Password {
			c.systemID = req.SystemID
			res.CommandStatus = data.ESME_ROK
		} else {
			res.CommandStatus = data.ESME_RINVPASWD
		}
		_, err = c.WritePDU(res)
		return err
	}
	return fmt.Errorf("bad request")
}
