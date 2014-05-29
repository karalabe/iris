// Iris - Decentralized cloud messaging
// Copyright (c) 2013 Project Iris. All rights reserved.
//
// Iris is dual licensed: you can redistribute it and/or modify it under the
// terms of the GNU General Public License as published by the Free Software
// Foundation, either version 3 of the License, or (at your option) any later
// version.
//
// The framework is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE.  See the GNU General Public License for
// more details.
//
// Alternatively, the Iris framework may be used in accordance with the terms
// and conditions contained in a signed written agreement between you and the
// author(s).

// Event handlers for both relay and carrier side messages. Almost all methods
// in this file are assumed to be running in a separate go routine! The only two
// exceptions are the tunnel data transfers, which need total ordering.

package relay

import (
	"errors"
	"log"
	"time"

	"github.com/project-iris/iris/config"
	"github.com/project-iris/iris/proto/iris"
)

// Forwards an app broadcast arriving from the Iris network to the attached app.
// Any error is considered a protocol violation.
func (r *relay) HandleBroadcast(msg []byte) {
	if err := r.sendBroadcast(msg); err != nil {
		log.Printf("relay: broadcast forward error: %v.", err)
		r.drop()
	}
}

// Forwards an app broadcast from the attached relay to the Iris network. Any
// error is considered a protocol violation.
func (r *relay) handleBroadcast(app string, msg []byte) {
	if err := r.iris.Broadcast(app, msg); err != nil {
		log.Printf("relay: broadcast error: %v.", err)
		r.drop()
	}
}

// Forwards a request arriving from the Iris network to the attached binding. A
// local timer is started to ensure a faulty client doesn't fill the node with
// stale requests.
func (r *relay) HandleRequest(request []byte, timeout time.Duration) ([]byte, error) {
	// Create a reply and error channel for the results
	repc := make(chan []byte, 1)
	errc := make(chan error, 1)

	r.reqLock.Lock()
	reqId := r.reqIdx
	r.reqIdx++
	r.reqReps[reqId] = repc
	r.reqErrs[reqId] = errc
	r.reqLock.Unlock()

	// Make sure the result channels are cleaned up
	defer func() {
		r.reqLock.Lock()
		delete(r.reqReps, reqId)
		delete(r.reqErrs, reqId)
		close(repc)
		close(errc)
		r.reqLock.Unlock()
	}()
	// Send the request
	if err := r.sendRequest(reqId, request, int(timeout.Nanoseconds()/1000000)); err != nil {
		log.Printf("relay: request error: %v.", err)
		r.drop()
		return nil, err
	}
	// Retrieve the results or fail if terminating
	select {
	case <-r.term:
		return nil, iris.ErrTerminating
	case <-time.After(timeout):
		return nil, iris.ErrTimeout
	case reply := <-repc:
		return reply, nil
	case err := <-errc:
		return nil, err
	}
}

// Forwards a request arriving from the attached binding to the Iris network, and
// waits for a reply to arrive back which can be forwarded.
func (r *relay) handleRequest(cluster string, id uint64, request []byte, timeout time.Duration) {
	reply, err := r.iris.Request(cluster, request, timeout)
	switch {
	case err == iris.ErrTimeout || err == iris.ErrTerminating:
		r.sendReply(id, nil, "")
	case err != nil:
		r.sendReply(id, nil, err.Error())
	default:
		r.sendReply(id, reply, "")
	}
}

// Forwards a reply arriving from the attached binding to the Iris network by
// looking up the pending request channel and if still live, injecting the result.
func (r *relay) handleReply(id uint64, reply []byte, fault string) {
	r.reqLock.RLock()
	defer r.reqLock.RUnlock()

	// Fetch the result channels
	repc, ok := r.reqReps[id]
	if !ok {
		return
	}
	errc, ok := r.reqErrs[id]
	if !ok {
		panic("reply channel available, error missing")
	}
	// Return either the reply or the fault
	if reply == nil && len(fault) == 0 {
		errc <- iris.ErrTimeout
	} else if reply == nil {
		errc <- errors.New(fault)
	} else {
		repc <- reply
	}
}

// Handler for a topic subscription. Forwards all published events to the app
// attached.
type subscriptionHandler struct {
	relay *relay
	topic string
}

// Forwards the arriving event from the Iris network to the attached app. Any
// error is considered a protocol violation.
func (s *subscriptionHandler) HandleEvent(msg []byte) {
	if err := s.relay.sendPublish(s.topic, msg); err != nil {
		log.Printf("relay: publish forward error: %v.", err)
		s.relay.drop()
	}
}

// Forwards a subscription event arriving from the attached app to the Iris node
// and creates a new subscription handler to process the arriving events. Any
// error is considered a protocol violation.
func (r *relay) handleSubscribe(topic string) {
	// Create the event forwarder
	handler := &subscriptionHandler{
		relay: r,
		topic: topic,
	}
	// Subscribe and drop connection in case of an error
	if err := r.iris.Subscribe(topic, handler); err != nil {
		log.Printf("relay: subscription error: %v.", err)
		r.drop()
	}
}

// Forwards a publish event arriving from the attached app to the Iris node. Any
// error is considered a protocol violation.
func (r *relay) handlePublish(topic string, msg []byte) {
	if err := r.iris.Publish(topic, msg); err != nil {
		log.Printf("relay: publish error: %v.", err)
		r.drop()
	}
}

// Forwards a subscription removel request arriving from the attached app to the
// Iris node. Any error is considered a protocol violation.
func (r *relay) handleUnsubscribe(topic string) {
	if err := r.iris.Unsubscribe(topic); err != nil {
		log.Printf("relay: unsubscription error: %v.", err)
		r.drop()
	}
}

// Forwards a tunneling request from the Iris network to the attached app. If no
// reply comes within some alloted time, the tunnel and connection are dropped.
func (r *relay) HandleTunnel(tun *iris.Tunnel) {
	// Allocate a temporary tunnel id
	r.tunLock.Lock()
	tmpId := r.tunIdx
	initChan := make(chan struct{}, 1)
	r.tunInit[tmpId] = initChan
	r.tunPend[tmpId] = tun
	r.tunIdx++
	r.tunLock.Unlock()

	// Send a tunneling request to the attached app
	if err := r.sendTunnelRequest(tmpId, config.RelayTunnelBuffer); err != nil {
		log.Printf("relay: tunnel request notification failed: %v.", err)
		r.drop()
	}
	// Wait for the final id and save the tunnel
	select {
	case <-time.After(time.Duration(config.RelayTunnelTimeout) * time.Millisecond):
		// Tunneling timed out, protocol violation
		log.Printf("relay: tunnel request timed out.")
		r.drop()
	case <-initChan:
		// Tunnel initialized, release timer
		r.tunLock.Lock()
		delete(r.tunInit, tmpId)
		delete(r.tunPend, tmpId)
		r.tunLock.Unlock()
	}
}

// Forwards a tunneling request from the attached application to the Iris node.
// After the successful setup or a timeout, the respective result is relayed
// back to the application.
func (r *relay) handleTunnelRequest(tunId uint64, app string, buf int, timeout time.Duration) {
	// Create the tunnel
	tun, err := r.iris.Tunnel(app, timeout)
	if err != nil {
		if err := r.sendTunnelReply(tunId, 0, true); err != nil {
			log.Printf("relay: tunnel timeout notification error: %v.", err)
			r.drop()
		}
		return
	}
	// Insert the tunnel into the tracked ones
	r.tunLock.Lock()
	tunnel := r.newTunnel(tunId, tun, config.RelayTunnelBuffer, buf)
	r.tunLive[tunId] = tunnel
	r.tunLock.Unlock()

	// Notify the attached app of the success
	if err := r.sendTunnelReply(tunId, config.RelayTunnelBuffer, false); err != nil {
		log.Printf("relay: tunnel success notification error: %v.", err)
		r.drop()
	}
	// Start the data transfer
	go tunnel.sender()
	go tunnel.receiver()
}

// Finalizes a tunnelling, notifies the tunneler of the success and starts the
// data flow.
func (r *relay) handleTunnelReply(tmpId uint64, tunId uint64, buf int) {
	r.tunLock.Lock()
	defer r.tunLock.Unlock()

	// Create the new relay tunnel
	tunnel := r.newTunnel(tunId, r.tunPend[tmpId], config.RelayTunnelBuffer, buf)
	r.tunLive[tunId] = tunnel

	// Signal the tunnel request of the successful initialization
	if initChan, ok := r.tunInit[tmpId]; ok {
		initChan <- struct{}{}
	}
	// Start the data transfer
	go tunnel.sender()
	go tunnel.receiver()
}

// Forwards a tunnel data packet from the attached app into the correct
// endpoint. Any errors at this point are considered protocol violations.
func (r *relay) handleTunnelSend(tunId uint64, msg []byte) {
	r.tunLock.RLock()
	defer r.tunLock.RUnlock()

	if tun, ok := r.tunLive[tunId]; ok {
		if err := tun.send(msg); err != nil {
			log.Printf("relay: tunnel send failed: %v.", err)
			r.drop()
		}
	}
}

// Forwards a tunnel data packet from the Iris network to the attached app.
func (r *relay) handleTunnelRecv(tunId uint64, msg []byte) {
	if err := r.sendTunnelData(tunId, msg); err != nil {
		log.Printf("relay: tunnel recv failed: %v.", err)
		r.drop()
	}
}

// Acknowledges the receipt of a tunneled message, permitting the sender to
// proceed.
func (r *relay) handleTunnelAck(tunId uint64) {
	r.tunLock.RLock()
	defer r.tunLock.RUnlock()

	if tun, ok := r.tunLive[tunId]; ok {
		if err := tun.ack(); err != nil {
			log.Printf("relay: tunnel ack failed: %v.", err)
			r.drop()
		}
	}
}

// Terminates the tunnel data transfer threads and notifies the remote endpoint.
func (r *relay) handleTunnelClose(tunId uint64, local bool) {
	// Remove the tunnel
	r.tunLock.Lock()
	tun, ok := r.tunLive[tunId]
	delete(r.tunLive, tunId)
	r.tunLock.Unlock()

	if ok {
		// In case of a local close, signal the remote endpoint
		if local {
			go tun.tun.Close()
		}
		// Terminate the tunnel transfers
		go tun.close()

		// Signal the application of termination
		if err := r.sendTunnelClose(tunId); err != nil {
			log.Printf("relay: tunnel close notification failed: %v", err)
			r.drop()
		}
	}
}
