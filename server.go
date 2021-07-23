package gosmpp

import (
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/dd1337/gosmpp/data"
	"github.com/dd1337/gosmpp/pdu"
	"go.uber.org/zap"
	"net"
	"sync"
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
}

type Server struct {
	sync.RWMutex
	addr string
	tls  *tls.Config

	accs  []ServerAccount
	conns []*boundConnection

	log *zap.Logger

	writeChan chan pdu.PDU
}

func NewServer(cfg ServerSettings, writeChan chan pdu.PDU) *Server {
	s := &Server{
		addr:      cfg.Address,
		tls:       cfg.TLS,
		accs:      cfg.Accounts,
		conns:     []*boundConnection{},
		log:       cfg.Logger,
		writeChan: writeChan,
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
	go s.startWrite()
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

	s.Lock()
	s.conns = append(s.conns, bc)
	s.Unlock()

	defer func() {
		s.Lock()
		s.removeConnection(c.systemID)
		s.Unlock()
	}()

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
			s.conns = remove(s.conns, i)
			fmt.Println("connections size", len(s.conns))
			return
		}
	}
}

func remove(s []*boundConnection, i int) []*boundConnection {
	if i == 0 {
		return s[1:]
	}
	if i == len(s)-1 {
		return s[:len(s)-1]
	}
	return append(s[:i-1], s[i:]...)
}

func (s *Server) bindConnection(c *Connection) (bc *boundConnection, err error) {
	logAddr := zap.String("addr", c.RemoteAddr().String())
	s.log.Debug("Binding connection", logAddr)
	var p pdu.PDU
	if p, err = pdu.Parse(c); err != nil {
		return
	}

	if req, ok := p.(*pdu.BindRequest); ok {
		var bound bool
		res := pdu.NewBindResp(*req)

		var acc ServerAccount
		acc, err = s.getAccount(req.SystemID)
		if err != nil {
			res.CommandStatus = data.ESME_RINVSYSID
		}

		if err == nil {
			accPassword := acc.Auth.Password
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
		}

		_, writeErr := c.WritePDU(res)
		if writeErr != nil {
			err = writeErr
		}

		if bound {
			bc = &boundConnection{
				Connection:     c,
				onPDU:          acc.OnPDU,
				onReceiveError: acc.OnReceiveError,
				log:            s.log,
			}
		}
		return
	}

	err = errors.New("bind request expected but received something else")

	return
}

func (s *Server) startWrite() {
	if s.writeChan == nil {
		s.log.Panic("Write chan not passed")
	}
	for {
		select {
		case p, ok := <-s.writeChan:
			for _, c := range s.conns {
				_, err := c.WritePDU(p)

				if err != nil {
					c.log.Error("Error writing to client", zap.Error(err))
				}
			}

			if !ok {
				s.log.Debug("Write channel closed")
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
