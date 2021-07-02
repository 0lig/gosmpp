package gosmpp

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dd1337/gosmpp/data"
	"github.com/dd1337/gosmpp/pdu"
)

type TestContext struct {
	reqests   []pdu.PDU
	responses []pdu.PDU
	mu        sync.Mutex
}

const serverAddress = "127.0.0.1:2775"
const serverUser = "test"
const serverPassword = "quest"

func TestServer(t *testing.T) {
	var wg sync.WaitGroup

	err := os.Setenv("SERVER_ADDR", serverAddress)
	if err != nil {
		panic(err)
	}

	tc := &TestContext{}
	go StartServer(tc)

	wg.Add(1)
	go sendingAndReceiveSMS(&wg)

	wg.Wait()

	requestCount := 0
	respCount := 0
	successRespCount := 0
	for _, req := range tc.reqests {
		requestCount++
		for _, res := range tc.responses {
			respCount++
			if req.GetSequenceNumber() == res.GetSequenceNumber() {
				successRespCount++
				fmt.Println("seq nr:", req.GetSequenceNumber(), "success:", res.IsOk())
			}
		}
	}

	if requestCount > respCount {
		t.Error("not enough responses")
	}

	if successRespCount < requestCount {
		t.Error("not all responses successfult")
	}
}

func getAccounts(tc *TestContext) []ServerAccount {
	return []ServerAccount{{
		Auth: &Auth{
			SystemID: serverUser,
			Password: serverPassword,
		},
		OnPDU: func(p pdu.PDU) (res pdu.PDU, err error) {
			tc.mu.Lock()
			tc.reqests = append(tc.reqests, p)
			res, err = handleServerReceive(p)
			tc.responses = append(tc.responses, res)
			tc.mu.Unlock()
			return
		},
		OnReceiveError: nil,
	}}
}

func makeServerSettings(tc *TestContext) ServerSettings {
	addr := os.Getenv("SERVER_ADDR")
	if addr == "" {
		panic("server address not defined")
	}
	return ServerSettings{
		Address:           addr,
		Accounts:          getAccounts(tc),
		OnConnectionError: handleConnectionError,
	}
}

func StartServer(tc *TestContext) {
	settings := makeServerSettings(tc)
	srv := NewServer(settings)
	srv.Start()
}

func handleConnectionError(err error) {
	fmt.Println("connection error", err)
}

func handleServerReceive(p pdu.PDU) (res pdu.PDU, err error) {
	switch pd := p.(type) {
	case *pdu.SubmitSM:
		res = pd.GetResponse()
		return
	}

	return nil, err
}

func sendingAndReceiveSMS(wg *sync.WaitGroup) {
	defer wg.Done()

	auth := Auth{
		SMSC:       serverAddress,
		SystemID:   serverUser,
		Password:   serverPassword,
		SystemType: "",
	}

	trans, err := NewSession(
		TRXConnector(NonTLSDialer, auth),
		Settings{
			EnquireLink: 5 * time.Second,

			ReadTimeout: 10 * time.Second,

			OnSubmitError: func(_ pdu.PDU, err error) {
				log.Fatal("SubmitPDU error:", err)
			},

			OnReceivingError: func(err error) {
				fmt.Println("Receiving PDU/Network error:", err)
			},

			OnRebindingError: func(err error) {
				fmt.Println("Rebinding but error:", err)
			},

			OnPDU: testHandlePDU(),

			OnClosed: func(state State) {
				fmt.Println(state)
			},
		}, 5*time.Second)
	if err != nil {
		log.Println(err)
	}
	defer func() {
		_ = trans.Close()
	}()

	// sending SMS(s)
	for i := 0; i < 5; i++ {
		if err = trans.Transceiver().Submit(testNewSubmitSM()); err != nil {
			fmt.Println(err)
		}
		time.Sleep(time.Second)
	}
}

func testHandlePDU() func(pdu.PDU, bool) {
	concatenated := map[uint8][]string{}
	return func(p pdu.PDU, _ bool) {
		switch pd := p.(type) {
		case *pdu.SubmitSMResp:
			fmt.Printf("SubmitSMResp:%+v\n", pd)

		case *pdu.GenericNack:
			fmt.Println("GenericNack Received")

		case *pdu.EnquireLinkResp:
			fmt.Println("EnquireLinkResp Received")

		case *pdu.DataSM:
			fmt.Printf("DataSM:%+v\n", pd)

		case *pdu.DeliverSM:
			fmt.Printf("DeliverSM:%+v\n", pd)
			log.Println(pd.Message.GetMessage())
			// region concatenated sms (sample code)
			message, err := pd.Message.GetMessage()
			if err != nil {
				log.Fatal(err)
			}
			totalParts, sequence, reference, found := pd.Message.UDH().GetConcatInfo()
			if found {
				if _, ok := concatenated[reference]; !ok {
					concatenated[reference] = make([]string, totalParts)
				}
				concatenated[reference][sequence-1] = message
			}
			if !found {
				log.Println(message)
			} else if parts, ok := concatenated[reference]; ok && isConcatenatedDone(parts, totalParts) {
				log.Println(strings.Join(parts, ""))
				delete(concatenated, reference)
			}
			// endregion
		}
	}
}

func testNewSubmitSM() *pdu.SubmitSM {
	// build up submitSM
	srcAddr := pdu.NewAddress()
	srcAddr.SetTon(5)
	srcAddr.SetNpi(0)
	_ = srcAddr.SetAddress("00" + "522241")

	destAddr := pdu.NewAddress()
	destAddr.SetTon(1)
	destAddr.SetNpi(1)
	_ = destAddr.SetAddress("99" + "522241")

	submitSM := pdu.NewSubmitSM().(*pdu.SubmitSM)
	submitSM.SourceAddr = srcAddr
	submitSM.DestAddr = destAddr
	_ = submitSM.Message.SetMessageWithEncoding("Đừng buồn thế dù ngoài kia vẫn mưa nghiễng rợi tý tỵ", data.UCS2)
	submitSM.ProtocolID = 0
	submitSM.RegisteredDelivery = 1
	submitSM.ReplaceIfPresentFlag = 0
	submitSM.EsmClass = 0

	return submitSM
}

func isConcatenatedDone(parts []string, total byte) bool {
	for _, part := range parts {
		if part != "" {
			total--
		}
	}
	return total == 0
}
