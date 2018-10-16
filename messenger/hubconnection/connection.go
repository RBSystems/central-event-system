package hubconnection

import (
	"fmt"
	"time"

	"github.com/byuoitav/central-event-system/hub/base"
	"github.com/byuoitav/central-event-system/hub/incomingconnection"
	"github.com/byuoitav/common/log"
	"github.com/byuoitav/common/nerr"
	"github.com/fatih/color"
	"github.com/gorilla/websocket"
)

const (
	// Interval to wait between retry attempts
	retryInterval = 3 * time.Second
)

//HubConnection is the connection from this receiver to a hub
type HubConnection struct {
	ID             string
	ConnectionType string

	writeChannel chan base.EventWrapper
	readChannel  chan base.EventWrapper

	conn    *websocket.Conn
	hubAddr string

	readDone     chan bool
	writeDone    chan bool
	lastPingTime time.Time
	state        string
}

//SendEvent will queue an event to be sent to the central hub
func (h *HubConnection) SendEvent(b base.EventWrapper) {
	h.writeChannel <- b
}

//ReadEvent requests the next available event from the queue
func (h *HubConnection) ReadEvent() base.EventWrapper {
	return <-h.readChannel
}

//ConnectToHub starts a connection to the hub for this hubconnection
func (h *HubConnection) ConnectToHub(hubAddress string) error {
	h.hubAddr = hubAddress

	// open connection with router
	err := h.openConnection()
	if err != nil {
		log.L.Warnf("Opening connection to hub failed, retrying...")

		h.readDone <- true
		h.writeDone <- true
		go h.retryConnection()

		return nerr.Create(fmt.Sprintf("failed to open connection to hub %v. retrying connection...", h.hubAddr), "connection-error")
	}

	// update state to good
	h.state = "good"
	log.L.Infof(color.HiGreenString("Successfully connected to hub %s. Starting pumps...", h.hubAddr))

	// start read/write pumps
	go h.startReadPump()
	go h.startWritePump()

	return nil
}

func (h *HubConnection) openConnection() error {
	// open connection to the router
	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(fmt.Sprintf("ws://%s/connect/%s", h.hubAddr, h.ConnectionType), nil)
	if err != nil {
		return nerr.Create(fmt.Sprintf("failed opening websocket with %v: %s", h.hubAddr, err), "connection-error")
	}

	h.conn = conn
	return nil
}

func (h *HubConnection) retryConnection() {
	// mark the connection as 'down'
	h.state = h.state + " retrying"

	log.L.Infof("[retry] Retrying connection, waiting for read and write pump to close before starting.")
	//wait for read to say i'm done.
	<-h.readDone
	log.L.Infof("[retry] Read pump closed")

	//wait for write to be done.
	<-h.writeDone
	log.L.Infof("[retry] Write pump closed")
	log.L.Infof("[retry] Retrying connection")

	//we retry
	err := h.openConnection()

	for err != nil {
		log.L.Infof("[retry] Retry failed, trying to connect to %s again in %v seconds.", h.hubAddr, retryInterval)
		time.Sleep(retryInterval)
		err = h.openConnection()
	}

	//start the pumps again
	log.L.Infof(color.HiGreenString("[Retry] Retry success. Starting pumps"))

	h.state = "good"
	go h.startReadPump()
	go h.startWritePump()

}

func (h *HubConnection) startReadPump() {
	defer func() {
		h.conn.Close()
		log.L.Warnf("Connection to hub %v is dying.", h.hubAddr)
		h.state = "down"

		h.readDone <- true
	}()

	h.conn.SetPingHandler(
		func(string) error {
			log.L.Infof("[%v] Ping!", h.hubAddr)
			h.conn.SetReadDeadline(time.Now().Add(incomingconnection.PingWait))
			h.conn.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(incomingconnection.WriteWait))

			//debugging purposes
			h.lastPingTime = time.Now()

			return nil
		})

	h.conn.SetReadDeadline(time.Now().Add(incomingconnection.PingWait))

	for {
		t, b, err := h.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.L.Errorf("Websocket closing: %v", err)
			}
			log.L.Errorf("Error: %v", err)
			return
		}

		if t != websocket.BinaryMessage {
			log.L.Warnf("Unknown message type %v", t)
			continue
		}

		//parse out room name
		m, er := base.ParseMessage(b)
		if er != nil {
			log.L.Warnf("Poorly formed message %s: %v", b, er.Error())
			continue
		}
		h.readChannel <- m
	}

}

func (h *HubConnection) startWritePump() {
	defer func() {
		h.conn.Close()
		log.L.Warnf("Connection to hub %v is dying. Trying to resurrect.", h.hubAddr)
		h.state = "down"

		h.writeDone <- true

		//try to reconnect
		h.retryConnection()
	}()

	for {
		select {
		case message, ok := <-h.writeChannel:
			if !ok {
				h.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(incomingconnection.WriteWait))
				return
			}

			err := h.conn.WriteMessage(websocket.BinaryMessage, base.PrepareMessage(message))
			if err != nil {
				log.L.Errorf("Problem writing message to socket: %v", err.Error())
				return
			}

		case <-h.readDone:
			// put it back in
			h.readDone <- true
			return
		}
	}

}
