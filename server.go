package gosmpp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/linxGnu/gosmpp/data"
	"github.com/linxGnu/gosmpp/pdu"
	"net"
)

type ServerSettings struct {
	Address           string
	Accounts          []ServerAccount
	OnConnectionError func(err error)
}

type ServerAccount struct {
	Auth           *Auth
	OnPDU          func(p pdu.PDU) (res pdu.PDU, err error)
	OnReceiveError func(err error)
}

type Server struct {
	addr string
	tls  *tls.Config

	accs  []ServerAccount
	conns []*boundConnection
}

func NewServer(cfg ServerSettings) *Server {
	s := &Server{
		addr:  cfg.Address,
		tls:   nil,
		accs:  cfg.Accounts,
		conns: []*boundConnection{},
	}
	return s
}

func (s *Server) getAccount(id string) (acc ServerAccount, err error) {
	for _, a := range s.accs {
		if a.Auth.SystemID == id {
			acc = a
			return
		}
	}
	return ServerAccount{}, fmt.Errorf("account %s not found\n", id)
}

func (s *Server) Start() error {
	fmt.Println("starting server on", s.addr)
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	for {
		var c net.Conn
		c, err = l.Accept()
		if err != nil {
			return err
		}
		go s.handleConn(c)
	}
}

type boundConnection struct {
	*Connection
	onPDU          func(p pdu.PDU) (res pdu.PDU, err error)
	onReceiveError func(err error)
}

func (s *Server) handleConn(conn net.Conn) {
	fmt.Println("incoming connection", conn.RemoteAddr().String())
	c := NewConnection(conn)

	accs := map[systemId]Auth{}
	for _, v := range s.accs {
		accs[systemId(v.Auth.SystemID)] = *v.Auth
	}

	bc, err := s.bindConnection(c)
	if err != nil {
		fmt.Println("bind error", err)
		return
	} else {
		s.conns = append(s.conns, bc)
		defer s.removeConnection(c)
	}

	err = bc.start()
	defer bc.Close()

	if err != nil {
		fmt.Println("connection error", err)
		return
	}
}

func (s *Server) removeConnection(conn *Connection) {
	for i, v := range s.conns {
		if v.systemID == conn.systemID {
			remove(s.conns, i)
			return
		}
	}
}

func remove(s []*boundConnection, i int) []*boundConnection {
	s[len(s)-1], s[i] = s[i], s[len(s)-1]
	return s[:len(s)-1]
}

type systemId string

func (s *Server) bindConnection(c *Connection) (bc *boundConnection, err error) {
	fmt.Println("binding connection")
	var (
		p pdu.PDU
	)

	if p, err = pdu.Parse(c); err != nil {
		return
	}

	if req, ok := p.(*pdu.BindRequest); ok {
		var acc ServerAccount
		acc, err = s.getAccount(req.SystemID)
		if err != nil {
			return
		}

		res := pdu.NewBindResp(*req)
		accPassword := acc.Auth.Password
		var bound bool
		if accPassword != "" {
			if req.Password == accPassword {
				acceptBind(c, req.SystemID, res)
				bound = true
			} else {
				res.CommandStatus = data.ESME_RINVPASWD
				err = errors.New("wrong password")
			}
		} else {
			acceptBind(c, req.SystemID, res)
			bound = true
		}
		_, err = c.WritePDU(res)
		if err != nil {
			return
		}

		if bound {
			bc = &boundConnection{
				Connection:     c,
				onPDU:          acc.OnPDU,
				onReceiveError: acc.OnReceiveError,
			}
		}
		return
	}

	return
}

func (c *boundConnection) start() error {
	fmt.Println("starting read")
	for {
		p, err := pdu.Parse(c)
		if err != nil {
			return err
		}
		defr := handleDefault(p)
		if defr != nil {
			_, err = c.WritePDU(*defr)

			if err != nil {
				c.onReceiveError(err)
				return err
			}
			continue
		}

		res, err := c.onPDU(p)
		if err != nil {
			c.onReceiveError(err)
			return err
		}

		_, err = c.WritePDU(res)

		if err != nil {
			c.onReceiveError(err)
			return err
		}
	}
}

func handleDefault(p pdu.PDU) (res *pdu.PDU) {
	switch pd := p.(type) {
	case *pdu.EnquireLink:
		r := pd.GetResponse()
		return &r
	case *pdu.Unbind:
		r := pd.GetResponse()
		return &r
	}
	return
}

func acceptBind(c *Connection, systemId string, res *pdu.BindResp) {
	c.systemID = systemId
	res.CommandStatus = data.ESME_ROK
}
