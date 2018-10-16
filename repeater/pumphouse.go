package repeater

import (
	"fmt"
	"net"
	"time"

	"github.com/byuoitav/common/db"
	"github.com/byuoitav/common/log"
	"github.com/byuoitav/common/nerr"
	"github.com/byuoitav/common/v2/events"
	"github.com/gorilla/websocket"
)

const (
	//TTL .
	TTL = 5 * time.Second

	//readBufferSize
	readBufferSize  = 1024
	writeBufferSize = 1024

	//port for the translators on the devices
	translatorport = "6998"
)

//PumpingStation .
type PumpingStation struct {
	conn websocket.Conn

	ID   string
	Room string

	remoteaddr string

	//internal channels
	readChannel  chan event.Event
	writeChannel chan event.Event

	readExit  chan bool
	writeExit chan bool
	errorChan chan error

	writeTimeout time.Time
	readTimeout  time.Time

	//external channels
	ReceiveChannel chan event.Event
	SendChannel    chan event.Event

	r *Repeater
}

//StartConnection takes a proc number, and will build the buffers, return it while asyncronously starting the connection
func StartConnection(proc, room string, r *Repeater) (*PumpingStation, *nerr.E) {

	toreturn := &pumpingStation{
		readChannel:    make(chan event.Event, readBufferSize),
		writeChannel:   make(chan event.Event, writeBufferSize),
		ReceiveChannel: r.HubSendBuffer,
		SendChannel:    make(chan event.Event, writeBufferSize),
		readExit:       make(chan bool, 1),
		writeExit:      make(chan bool, 1),
		errorChan:      make(chan error, 2),
		ID:             proc,
		Room:           room,
		r:              r,
	}

	go toreturn.start()

	return toreturn, nil
}

func buildFromConnection(proc, room string, r *Repeater, conn *websocket.Conn) (*PumpingStation, *nerr.E) {

	toreturn := &pumpingStation{
		readChannel:    make(chan event.Event, readBufferSize),
		writeChannel:   make(chan event.Event, writeBufferSize),
		ReceiveChannel: r.HubSendBuffer,
		SendChannel:    make(chan event.Event, writeBufferSize),
		readExit:       make(chan bool, 1),
		writeExit:      make(chan bool, 1),
		errorChan:      make(chan error, 2),
		ID:             proc,
		Room:           room,
		r:              r,
		conn:           conn,
		remoteaddr:     conn.RemoteAddr().String(),
	}

	go toreturn.startReadPump()
	go toreturn.startWritePump()

	//we assume that the caller will start the pumper
	return toreturn, nil
}

func (c *PumpingStation) start() {
	//we need to get the address of the processor I want to talk to a
	dev, err := db.GetDB().GetDevice(c.ID)
	if er != nil {
		log.L.Errorf("Couldn't retrieve device %v from database: %v", c.ID, er.Error())
		c.r.UnregisterConnection(c.ID)
		return
	}

	err := c.openConn(dev.Address)
	if err != nil {
		log.L.Errorf("couldn't initializle for %v: %v", c.ID, err.Error())
		c.r.UnregisterConnection(c.ID)
		return
	}

	go c.startReadPump()
	go c.startWritePump()
	c.startpumper()
}

func (c *PumpingStation) openConn(addr string) *nerr.E {
	log.L.Debugf("Starting connection with %v", addr)

	c.remoteaddr = dev.Address

	dialer := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(fmt.Sprintf("ws://%s:%s/repeaterconn", addr, translatorport), nil)
	if err != nil {
		return nerr.Create(fmt.Sprintf("failed opening websocket with %v: %s", addr, err), "connection-error")
	}
	log.L.Debugf("Connection started with %v", addr)

	c.conn = conn
	return nil
}

//We don't try to re-establish this one, nor do we worry about ping/pong joy - we're alive until one of us closes it - hopefully 5 seconds of inactivity
func (c *PumpingStation) startReadPump() {

	c.conn.SetReadDeadline(time.Now().Add(TTL))
	for {
		var event events.Event
		t, b, err := c.conn.ReadJSON(&event)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				log.L.Errorf("[%v] Websocket closing: %v", c.ID, err)
			} else {
				netErr, ok := err.(net.Error)
				if ok && netErr.Timeout() {
					select {
					case <-readExit:
						return
					default:
						c.conn.SetReadDeadline(time.Now().Add(TTL))
						continue
					}
				}
			}
			log.L.Debugf("[%v] Returning", c.ID, err)
			c.errorChan <- err
			return
		}

		c.readChannel <- m

		c.conn.SetReadDeadline(time.Now().Add(TTL))
	}
}

func (c *PumpingStation) startWritePump() {

	c.conn.SetWriteDeadline = time.Now().Add(TTL)

	for {
		select {
		case msg <- c.writeChannel:
			//in the case of the write channel we just write it down the socket
			err := c.conn.WriteJSON(msg)
			if err != nil {
				log.L.Warnf("[%v} Problem writing message: %v", c.ID, err.Error())
				c.errorChan <- err
				return
			}
			c.conn.SetWriteDeadline = time.Now().Add(TTL)

		case <-c.writeExit:
			return
		}
	}
}

func (c *PumpingStation) startPumper() {
	defer func() {
		r.UnregisterConnection(c.ID)

		c.writeExit <- true
		c.readExit <- true

		time.Sleep(TTL)
		c.conn.Close()
	}()

	c.readTimeout = time.Now().Add(TTL)
	c.writeTimeout = time.Now().Add(TTL)

	//start our ticker
	t := time.NewTicker(TTL)
	select {
	case <-t.C:
		//check to see if read and write are after now
		if time.Now().After(c.readTimeout) && time.Now().After(c.writeTimeout) {
			//time to leave
			return
		}

	case err := <-c.errorChan:
		//there was an error
		log.L.Infof("[%v] error: %v. Closing..", c.ID, err.Error())
		return

	case e := <-c.SendChannel:
		c.writeTimeout = time.Now().Add(TTL)
		c.writeChannel <- e

	case e := <-c.readChannel:
		c.readTimeout = time.Now().Add(TTL)
		c.ReceiveChannel <- e
	}

}

//SendEvent .
func (c *PumpingStation) SendEvent(e event.Event) {
	c.SendChannel <- e
}
