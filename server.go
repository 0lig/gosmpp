package gosmpp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/dd1337/gosmpp/data"
	"github.com/dd1337/gosmpp/pdu"
	"go.uber.org/zap"
	"net"
)

type ServerSettings struct {
	Address           string
	Accounts          []ServerAccount
	OnConnectionError func(err error)
	TLS               *tls.Config
	Logger            *zap.Logger
}

type ServerAccount struct {
	Auth           *Auth
	OnPDU          func(p pdu.PDU) (res pdu.PDU, err error)
	OnReceiveError func(err error)
	WriteChan      chan pdu.PDU
}

type Server struct {
	addr string
	tls  *tls.Config

	accs  []ServerAccount
	conns []*boundConnection

	log *zap.Logger
}

func NewServer(cfg ServerSettings) *Server {
	s := &Server{
		addr:  cfg.Address,
		tls:   cfg.TLS,
		accs:  cfg.Accounts,
		conns: []*boundConnection{},
		log:   cfg.Logger,
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
	s.log.Info("Starting server", zap.String("addr", s.addr))
	var (
		l   net.Listener
		err error
	)
	if s.tls != nil {
		l, err = tls.Listen("tcp", s.addr, s.tls)
	} else {
		l, err = net.Listen("tcp", s.addr)
	}
	if err != nil {
		return err
	}
	for {
		var c net.Conn
		c, err = l.Accept()
		if err == nil {
			go s.handleConn(c)
		} else {
			s.log.Error(err.Error())
		}
	}
}

type boundConnection struct {
	*Connection
	onPDU          func(p pdu.PDU) (res pdu.PDU, err error)
	onReceiveError func(err error)
	log            *zap.Logger
	writeChan      chan pdu.PDU
}

func (s *Server) handleConn(conn net.Conn) {
	logAddr := zap.String("addr", conn.RemoteAddr().String())
	s.log.Debug("Incoming connection", logAddr)
	c := NewConnection(conn)

	bc, err := s.bindConnection(c)
	if err != nil {
		s.log.Debug("Bind error", logAddr, zap.Error(err))
		_ = conn.Close()
		return
	}
	logSysId := zap.String("systemId", bc.systemID)
	s.log.Info("Connection bound", logAddr, logSysId)

	s.conns = append(s.conns, bc)
	defer s.removeConnection(c.systemID)
	go bc.startWrite()
	err = bc.startRead()
	s.log.Info("Connection stopped reading", logAddr, logSysId)
	defer bc.Close()
	if err != nil {
		s.log.Error("Connection error", logAddr, logSysId, zap.Error(err))
		return
	}
}

func (s *Server) removeConnection(id string) {
	for i, v := range s.conns {
		if v.systemID == id {
			remove(s.conns, i)
			return
		}
	}
}

func remove(s []*boundConnection, i int) []*boundConnection {
	s[len(s)-1], s[i] = s[i], s[len(s)-1]
	return s[:len(s)-1]
}

func (s *Server) bindConnection(c *Connection) (bc *boundConnection, err error) {
	logAddr := zap.String("addr", c.RemoteAddr().String())
	s.log.Debug("Binding connection", logAddr)
	var p pdu.PDU
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
				log:            s.log,
				writeChan:      acc.WriteChan,
			}
		}
		return
	}

	err = errors.New("bind request expected but received something else")

	return
}

func (c *boundConnection) startWrite() {
	if c.writeChan == nil {
		c.log.Panic("Write chan not passed")
	}
	for {
		select {
		case p, ok := <-c.writeChan:
			_, err := c.WritePDU(p)
			if err != nil {
				c.log.Error("Error writing to server", zap.Error(err))
			}
			if !ok {
				c.log.Debug("Write channel closed")
				return
			}
		}
	}
}

func (c *boundConnection) startRead() error {
	logAddr := zap.String("addr", c.conn.RemoteAddr().String())
	logSysId := zap.String("systemId", c.systemID)

	c.log.Debug("Starting read", logAddr, logSysId)
	for {
		p, err := pdu.Parse(c)
		c.log.Debug("Incoming PDU", logAddr, logSysId)
		if err != nil {
			return err
		}
		defaultResponse, stop := handleDefault(p)
		if defaultResponse != nil {
			_, err = c.WritePDU(*defaultResponse)

			if err != nil {
				c.onReceiveError(err)
				return err
			}
			continue
		}

		if stop {
			c.log.Debug("Unbind received, stopping read", logAddr, logSysId)
			break
		}

		res, err := c.onPDU(p)
		if err != nil {
			c.onReceiveError(err)
			return err
		}

		if res != nil {
			_, err = c.WritePDU(res)

			if err != nil {
				c.onReceiveError(err)
				return err
			}
		}
	}
	return nil
}

func handleDefault(p pdu.PDU) (res *pdu.PDU, stop bool) {
	switch pd := p.(type) {
	case *pdu.EnquireLink:
		r := pd.GetResponse()
		res = &r
		return
	case *pdu.Unbind:
		r := pd.GetResponse()
		res = &r
		stop = true
		return
	}
	return
}

func acceptBind(c *Connection, systemId string, res *pdu.BindResp) {
	c.systemID = systemId
	res.CommandStatus = data.ESME_ROK
}
