package gosmpp

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/linxGnu/gosmpp/pdu"
)

const (
	TestAddr = ":30303"
)

func TestSMPPServer(t *testing.T) {
	// server creation
	srvPduReceived := make(chan pdu.PDU, 10)
	settings := Settings{
		ReadTimeout: time.Second,
		OnPDU:       func(p pdu.PDU, responded bool) { srvPduReceived <- p },
	}
	srv := NewServer("serverID", TestAddr, settings)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			t.Errorf("Expected no error on server close %v", err)
		}
	}()
	select {
	case <-srv.GetReadinessChan():
	case <-time.After(time.Second):
		t.Error("Timeout while waiting server readiness")
	}

	// TRX client creation
	trxPduReceived := make(chan pdu.PDU, 10)
	trx, err := NewSession(TRXConnector(NonTLSDialer, Auth{
		SMSC:       TestAddr,
		SystemID:   "1234",
		Password:   "",
		SystemType: "",
	}), Settings{
		ReadTimeout: time.Second,
		OnPDU: func(p pdu.PDU, responded bool) {
			trxPduReceived <- p
		},
		OnClosed: func(state State) {
			if state == UnbindClosing {
				trxPduReceived <- pdu.NewUnbind()
			}
		},
	}, 0)
	require.NoError(t, err)

	// TX client creation
	txPduReceived := make(chan pdu.PDU, 10)
	tx, err := NewSession(TXConnector(NonTLSDialer, Auth{
		SMSC:       TestAddr,
		SystemID:   "1235",
		Password:   "",
		SystemType: "",
	}), Settings{
		ReadTimeout: time.Second,
		OnPDU: func(p pdu.PDU, responded bool) {
			txPduReceived <- p
		},
		OnClosed: func(state State) {
			if state == UnbindClosing {
				txPduReceived <- pdu.NewUnbind()
			}
		},
	}, 0)
	require.NoError(t, err)

	err = trx.Transceiver().Submit(newSubmitSM("1234"))

	require.NoError(t, err)
	p := waitForPDU(t, srvPduReceived)
	assert.IsType(t, &pdu.SubmitSM{}, p)
	p = waitForPDU(t, trxPduReceived)
	assert.IsType(t, &pdu.SubmitSMResp{}, p)

	err = tx.Transmitter().Submit(newSubmitSM("1235"))

	require.NoError(t, err)
	p = waitForPDU(t, srvPduReceived)
	assert.IsType(t, &pdu.SubmitSM{}, p)
	p = waitForPDU(t, txPduReceived)
	assert.IsType(t, &pdu.SubmitSMResp{}, p)

	go func() {
		err = srv.Close()
		require.NoError(t, err)
	}()

	p = waitForPDU(t, trxPduReceived)
	assert.IsType(t, &pdu.Unbind{}, p)
	p = waitForPDU(t, txPduReceived)
	assert.IsType(t, &pdu.Unbind{}, p)
}

func TestSMPPServer_acceptOnlyOneSessionBySystemID(t *testing.T) {
	// server creation
	srvPduReceived := make(chan pdu.PDU, 10)
	settings := Settings{
		ReadTimeout: time.Second,
		OnPDU:       func(p pdu.PDU, responded bool) { srvPduReceived <- p },
	}
	srv := NewServer("servedID", TestAddr, settings)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			t.Errorf("Expected no error on server close %v", err)
		}
	}()
	select {
	case <-srv.GetReadinessChan():
	case <-time.After(time.Second):
		t.Error("Timeout while waiting server readiness")
	}

	// TRX client creation
	_, err := NewSession(TRXConnector(NonTLSDialer, Auth{
		SMSC:     TestAddr,
		SystemID: "1234",
	}), Settings{ReadTimeout: time.Second}, 0)
	require.NoError(t, err)

	_, err = NewSession(TRXConnector(NonTLSDialer, Auth{
		SMSC:     TestAddr,
		SystemID: "1234",
	}), Settings{ReadTimeout: time.Second}, 0)
	assert.Error(t, err, "NewSession should return an error with the same SystemID")

	_ = srv.Close()
}

func TestSMPPServer_DeliverSM(t *testing.T) {
	// server creation
	srvPduReceived := make(chan pdu.PDU, 10)
	settings := Settings{
		ReadTimeout: time.Second,
		OnPDU:       func(p pdu.PDU, responded bool) { srvPduReceived <- p },
	}
	srv := NewServer("serverID", TestAddr, settings)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			t.Errorf("Expected no error on server close %v", err)
		}
	}()
	select {
	case <-srv.GetReadinessChan():
	case <-time.After(time.Second):
		t.Error("Timeout while waiting server readiness")
	}

	// TRX client creation
	trxPduReceived := make(chan pdu.PDU, 10)
	_, err := NewSession(TRXConnector(NonTLSDialer, Auth{
		SMSC:       TestAddr,
		SystemID:   "1234",
		Password:   "",
		SystemType: "",
	}), Settings{
		ReadTimeout: time.Second,
		OnPDU: func(p pdu.PDU, responded bool) {
			trxPduReceived <- p
		},
		OnClosed: func(state State) {
			if state == UnbindClosing {
				trxPduReceived <- pdu.NewUnbind()
			}
		},
	}, 0)
	require.NoError(t, err)

	sm := pdu.NewDeliverSM().(*pdu.DeliverSM)
	sm.Message, _ = pdu.NewShortMessage("Hello")
	err = srv.DeliverSM("1234", sm)

	p := waitForPDU(t, trxPduReceived)
	assert.IsType(t, &pdu.DeliverSM{}, p)
	msg, err := (p.(*pdu.DeliverSM)).Message.GetMessage()
	require.NoError(t, err)
	assert.Equal(t, "Hello", msg)
}

func waitForPDU(t *testing.T, c <-chan pdu.PDU) pdu.PDU {
	t.Helper()
	select {
	case p := <-c:
		return p
	case <-time.After(time.Second):
		t.Fatal("timeout while waiting PDU")
		return nil
	}
}
