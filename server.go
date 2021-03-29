package gosmpp

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linxGnu/gosmpp/data"
	"github.com/linxGnu/gosmpp/pdu"
)

const keepAlivePeriod = 3 * time.Minute

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted connections
// Useful to deal with dead TCP connections when ESME does not send EnquireLink
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(keepAlivePeriod)
	return tc, nil
}

type Server struct {
	Addr     string
	SystemID string
	Settings Settings

	sessionsWg    sync.WaitGroup
	listener      net.Listener
	readinessChan chan struct{}
	sessions      map[string]*ServerSession
	sessionsMutex sync.Mutex
	closing       atomic.Value
}

func NewServer(systemID string, addr string, settings Settings) *Server {
	return &Server{
		SystemID:      systemID,
		Addr:          addr,
		Settings:      settings,
		readinessChan: make(chan struct{}),
		sessions:      make(map[string]*ServerSession),
	}
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls serve to handle SMPP requests on incoming connections.
//
// If srv.Addr is blank, ":2775" is used.
//
// ListenAndServe always returns nil on Close.
func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":2775"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	return srv.serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}

// serve accepts incoming connections and starts SMPP sessions.
func (srv *Server) serve(ln net.Listener) error {
	defer ln.Close()

	srv.listener = ln
	var tempDelay time.Duration // how long to sleep on accept failure
	srv.readinessChan <- struct{}{}

	for {
		conn, err := ln.Accept()
		if err != nil {
			if srv.isClosing() {
				return nil
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				time.Sleep(tempDelay)
				continue
			}

			return err
		}
		tempDelay = 0

		if srv.isClosing() {
			return nil
		}

		srv.sessionsWg.Add(1)
		go func(settings Settings, conn net.Conn) {
			defer srv.sessionsWg.Done()
			session, err := srv.acceptServerSession(conn, settings)
			if err != nil {
				return
			}
			srv.addSession(session)

			<-session.NotifyClosed()
			srv.removeSession(session)
		}(srv.Settings, conn)

	}
}

func (srv *Server) isClosing() bool {
	return srv.closing.Load() != nil
}

func (srv *Server) Close() error {
	srv.closing.Store(true)

	srv.sessionsMutex.Lock()
	for _, sess := range srv.sessions {
		sess.Close()
	}
	srv.sessionsMutex.Unlock()
	srv.sessionsWg.Wait()

	err := srv.listener.Close()
	return err
}

func (srv *Server) GetReadinessChan() <-chan struct{} {
	return srv.readinessChan
}

func (srv *Server) addSession(sess *ServerSession) {
	srv.sessionsMutex.Lock()
	defer srv.sessionsMutex.Unlock()

	srv.sessions[sess.SystemID] = sess
}

func (srv *Server) removeSession(sess *ServerSession) {
	srv.sessionsMutex.Lock()
	defer srv.sessionsMutex.Unlock()

	delete(srv.sessions, sess.SystemID)
}

func (srv *Server) getSession(systemID string) *ServerSession {
	srv.sessionsMutex.Lock()
	defer srv.sessionsMutex.Unlock()

	return srv.sessions[systemID]
}

func (srv *Server) acceptServerSession(conn net.Conn, settings Settings) (*ServerSession, error) {
	// create wrapped connection
	c := NewConnection(conn)

	var (
		pd *pdu.BindRequest
	)

	// catching bind
	for {
		p, err := pdu.Parse(c)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}

		var ok bool
		if pd, ok = p.(*pdu.BindRequest); ok {
			if pd.SystemID == "" {
				resp := pdu.NewBindResp(*pd)
				resp.CommandStatus = data.ESME_RINVSYSID
				if _, err := c.WritePDU(resp); err != nil {
					_ = conn.Close()
					return nil, err
				}

				return nil, errors.New("invalid ESME systemID")
			}

			if sess := srv.getSession(pd.SystemID); sess != nil {
				resp := pdu.NewBindResp(*pd)
				resp.CommandStatus = data.ESME_RALYBND
				if _, err := c.WritePDU(resp); err != nil {
					_ = conn.Close()
					return nil, err
				}

				return nil, fmt.Errorf("ESME session already bound with systemID %s", pd.SystemID)
			}

			resp := pdu.NewBindResp(*pd)
			resp.CommandStatus = data.ESME_ROK
			resp.SystemID = srv.SystemID
			if _, err := c.WritePDU(resp); err != nil {
				_ = conn.Close()
				return nil, err
			}
			break
		}
	}

	trx := newTransceivable(c, settings)
	s := &ServerSession{
		SystemID:     pd.SystemID,
		settings:     &settings,
		trx:          trx,
		notifyClosed: make(chan struct{}),
	}
	return s, nil
}

func (srv *Server) DeliverSM(systemID string, sm *pdu.DeliverSM) error {
	session := srv.getSession(systemID)
	if session == nil {
		return nil
	}

	return session.trx.Submit(sm)
}

type Handler func(s *ServerSession, p pdu.PDU)

type ServerSession struct {
	settings     *Settings
	trx          *transceivable
	notifyClosed chan struct{}
	SystemID     string
}

func (s *ServerSession) Close() {
	_ = s.trx.Close()
	close(s.notifyClosed)
}

func (s *ServerSession) NotifyClosed() <-chan struct{} {
	return s.notifyClosed
}
