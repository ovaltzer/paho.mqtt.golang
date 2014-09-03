/*
 * Copyright (c) 2013 IBM Corp.
 *
 * All rights reserved. This program and the accompanying materials
 * are made available under the terms of the Eclipse Public License v1.0
 * which accompanies this distribution, and is available at
 * http://www.eclipse.org/legal/epl-v10.html
 *
 * Contributors:
 *    Seth Hoenig
 *    Allan Stockdill-Mander
 *    Mike Robertson
 */

package mqtt

import (
	"code.google.com/p/go.net/websocket"
	"crypto/tls"
	. "github.com/alsm/hrotti/packets"
	//"io"
	"net"
	"net/url"
	"reflect"
	"time"
)

func openConnection(uri *url.URL, tlsc *tls.Config) (conn net.Conn, err error) {
	switch uri.Scheme {
	case "ws":
		conn, err = websocket.Dial(uri.String(), "mqtt", "ws://localhost")
		if err != nil {
			return
		}
		conn.(*websocket.Conn).PayloadType = websocket.BinaryFrame
	case "tcp":
		conn, err = net.Dial("tcp", uri.Host)
	case "ssl":
		fallthrough
	case "tls":
		fallthrough
	case "tcps":
		conn, err = tls.Dial("tcp", uri.Host, tlsc)
	}
	return
}

// This function is only used for receiving a connack
// when the connection is first started.
// This prevents receiving incoming data while resume
// is in progress if clean session is false.
func connect(c *MqttClient) byte {
	DEBUG.Println(NET, "connect started")

	ca, err := ReadPacket(c.conn)
	if err != nil {
		ERROR.Println(NET, "connect got error", err)
		c.errors <- err
		return CONN_NETWORK_ERROR
	}
	msg := ca.(*ConnackPacket)

	if msg == nil || msg.FixedHeader.MessageType != CONNACK {
		close(c.begin)
		ERROR.Println(NET, "received msg that was nil or not CONNACK")
	} else {
		DEBUG.Println(NET, "received connack")
	}
	return msg.ReturnCode
}

// actually read incoming messages off the wire
// send Message object into ibound channel
func incoming(c *MqttClient) {

	var err error
	var cp ControlPacket

	DEBUG.Println(NET, "incoming started")

	for {
		if cp, err = ReadPacket(c.conn); err != nil {
			break
		}
		DEBUG.Println(NET, "Received Message")
		c.ibound <- cp

		// var rerr error
		// var msg *Message
		// msgType := make([]byte, 1)
		// DEBUG.Println(NET, "incoming waiting for network data")
		// msgType[0], rerr = c.bufferedConn.ReadByte()
		// if rerr != nil {
		// 	err = rerr
		// 	break
		// }
		// bytes, remLen := decodeRemlenFromNetwork(c.bufferedConn)
		// fixedHeader := make([]byte, len(bytes)+1)
		// copy(fixedHeader, append(msgType, bytes...))
		// if remLen > 0 {
		// 	data := make([]byte, remLen)
		// 	DEBUG.Println(NET, remLen, "more incoming bytes to read")
		// 	_, rerr = io.ReadFull(c.bufferedConn, data)
		// 	if rerr != nil {
		// 		err = rerr
		// 		break
		// 	}
		// 	DEBUG.Println(NET, "data:", data)
		// 	msg = decode(append(fixedHeader, data...))
		// } else {
		// 	msg = decode(fixedHeader)
		// }
		// if msg != nil {
		// 	DEBUG.Println(NET, "incoming received inbound message, type", msg.msgType())
		// 	c.ibound <- msg
		// } else {
		// 	CRITICAL.Println(NET, "incoming msg was nil")
		// }
	}
	// We received an error on read.
	// If disconnect is in progress, swallow error and return
	select {
	case <-c.stop:
		DEBUG.Println(NET, "incoming stopped")
		return
		// Not trying to disconnect, send the error to the errors channel
	default:
		ERROR.Println(NET, "incoming stopped with error")
		c.errors <- err
		return
	}
}

// receive a Message object on obound, and then
// actually send outgoing message to the wire
func outgoing(c *MqttClient) {

	DEBUG.Println(NET, "outgoing started")

	for {
		DEBUG.Println(NET, "outgoing waiting for an outbound message")
		select {
		case msg := <-c.obound:
			if msg.Qos != 0 && msg.MessageID == 0 {
				msg.MessageID = c.options.mids.getId()
			}
			//persist_obound(c.persist, msg)

			if c.options.writeTimeout > 0 {
				c.conn.SetWriteDeadline(time.Now().Add(c.options.writeTimeout))
			}

			if err := msg.Write(c.conn); err != nil {
				ERROR.Println(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}

			if c.options.writeTimeout > 0 {
				// If we successfully wrote, we don't want the timeout to happen during an idle period
				// so we reset it to infinite.
				c.conn.SetWriteDeadline(time.Time{})
			}

			// msgtype := msg.FixedHeader.MessageType
			// if (msg.Qos == 0) &&
			// 	(msgtype == PUBLISH || msgtype == SUBSCRIBE || msgtype == UNSUBSCRIBE) {
			// 	c.receipts.get(msg.MessageID) <- Receipt{}
			// 	c.receipts.end(msg.MessageID)
			// }
			c.lastContact.update()
			DEBUG.Println(NET, "obound wrote msg, id:", msg.MessageID)
		case msg := <-c.oboundP:
			msgtype := reflect.TypeOf(msg)
			switch msg.(type) {
			case *SubscribePacket:
				msg.(*SubscribePacket).MessageID = c.options.mids.getId()
			case *UnsubscribePacket:
				msg.(*UnsubscribePacket).MessageID = c.options.mids.getId()
			}
			DEBUG.Println(NET, "obound priority msg to write, type", msgtype)
			if err := msg.Write(c.conn); err != nil {
				ERROR.Println(NET, "outgoing stopped with error")
				c.errors <- err
				return
			}
			c.lastContact.update()
			switch msg.(type) {
			case *DisconnectPacket:
				DEBUG.Println(NET, "outbound wrote disconnect, now closing connection")
				c.conn.Close()
				return
			}
		}
	}
}

// receive Message objects on ibound
// store messages if necessary
// send replies on obound
// delete messages from store if necessary
func alllogic(c *MqttClient) {

	DEBUG.Println(NET, "logic started")

	for {
		DEBUG.Println(NET, "logic waiting for msg on ibound")

		select {
		case msg := <-c.ibound:
			DEBUG.Println(NET, "logic got msg on ibound")
			//persist_ibound(c.persist, msg)
			switch msg.(type) {
			case *PingrespPacket:
				DEBUG.Println(NET, "received pingresp")
				c.pingOutstanding = false
			case *SubackPacket:
				sa := msg.(*SubackPacket)
				DEBUG.Println(NET, "received suback, id:", sa.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(sa.MessageID)
			case *UnsubackPacket:
				ua := msg.(*UnsubackPacket)
				DEBUG.Println(NET, "received unsuback, id:", ua.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(ua.MessageID)
			case *PublishPacket:
				pp := msg.(*PublishPacket)
				DEBUG.Println(NET, "received publish, msgId:", pp.MessageID)
				DEBUG.Println(NET, "putting msg on onPubChan")
				switch pp.Qos {
				case 2:
					c.options.incomingPubChan <- pp
					DEBUG.Println(NET, "done putting msg on incomingPubChan")
					pr := NewControlPacket(PUBREC).(*PubrecPacket)
					pr.MessageID = pp.MessageID
					DEBUG.Println(NET, "putting pubrec msg on obound")
					c.oboundP <- pr
					DEBUG.Println(NET, "done putting pubrec msg on obound")
				case 1:
					c.options.incomingPubChan <- pp
					DEBUG.Println(NET, "done putting msg on incomingPubChan")
					pa := NewControlPacket(PUBACK).(*PubackPacket)
					pa.MessageID = pp.MessageID
					DEBUG.Println(NET, "putting puback msg on obound")
					c.oboundP <- pa
					DEBUG.Println(NET, "done putting puback msg on obound")
				case 0:
					select {
					case c.options.incomingPubChan <- pp:
						DEBUG.Println(NET, "done putting msg on incomingPubChan")
					case err, ok := <-c.errors:
						DEBUG.Println(NET, "error while putting msg on pubChanZero")
						// We are unblocked, but need to put the error back on so the outer
						// select can handle it appropriately.
						if ok {
							go func(errVal error, errChan chan error) {
								errChan <- errVal
							}(err, c.errors)
						}
					}
				}
			case *PubackPacket:
				pa := msg.(*PubackPacket)
				DEBUG.Println(NET, "received puback, id:", pa.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(pa.MessageID)
			case *PubrecPacket:
				prec := msg.(*PubrecPacket)
				DEBUG.Println(NET, "received pubrec, id:", prec.MessageID)
				prel := NewControlPacket(PUBREL).(*PubrelPacket)
				prel.MessageID = prec.MessageID
				select {
				case c.oboundP <- prel:
				case <-time.After(time.Second):
				}
			case *PubrelPacket:
				pr := msg.(*PubrelPacket)
				DEBUG.Println(NET, "received pubrel, id:", pr.MessageID)
				pc := NewControlPacket(PUBCOMP).(*PubcompPacket)
				pc.MessageID = pr.MessageID
				select {
				case c.oboundP <- pc:
				case <-time.After(time.Second):
				}
			case *PubcompPacket:
				pc := msg.(*PubcompPacket)
				DEBUG.Println(NET, "received pubcomp, id:", pc.MessageID)
				// c.receipts.get(msg.MsgId()) <- Receipt{}
				// c.receipts.end(msg.MsgId())
				go c.options.mids.freeId(pc.MessageID)
			}
		case <-c.stop:
			WARN.Println(NET, "logic stopped")
			return
		case err := <-c.errors:
			c.connected = false
			ERROR.Println(NET, "logic got error")
			// clean up go routines
			// incoming most likely stopped if outgoing stopped,
			// but let it know to stop anyways.
			close(c.options.stopRouter)
			close(c.stop)
			c.conn.Close()

			// Call onConnectionLost or default error handler
			go c.options.onconnlost(c, err)
			return
		}
	}
}
