package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pborman/uuid"

	"github.com/gorilla/websocket"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

// DevLXDServer represents an instance of an devlxd event server.
type DevLXDServer struct {
	serverCommon

	listeners map[string]*DevLXDListener
}

// NewDevLXDServer returns a new devlxd event server.
func NewDevLXDServer(debug bool, verbose bool) *DevLXDServer {
	server := &DevLXDServer{
		serverCommon: serverCommon{
			debug:   debug,
			verbose: verbose,
		},
		listeners: map[string]*DevLXDListener{},
	}

	return server
}

// AddListener creates and returns a new event listener.
func (s *DevLXDServer) AddListener(instanceID int, connection *websocket.Conn, messageTypes []string) (*DevLXDListener, error) {
	ctx, ctxCancel := context.WithCancel(context.Background())

	listener := &DevLXDListener{
		listenerCommon: listenerCommon{
			Conn:         connection,
			messageTypes: messageTypes,
			localOnly:    true,
			ctx:          ctx,
			ctxCancel:    ctxCancel,
			id:           uuid.New(),
		},
		instanceID: instanceID,
	}

	s.lock.Lock()
	defer s.lock.Unlock()

	if s.listeners[listener.id] != nil {
		return nil, fmt.Errorf("A listener with ID %q already exists", listener.id)
	}

	s.listeners[listener.id] = listener

	go listener.heartbeat()

	return listener, nil
}

// Send broadcasts a custom event.
func (s *DevLXDServer) Send(instanceID int, eventType string, eventMessage interface{}) error {
	encodedMessage, err := json.Marshal(eventMessage)
	if err != nil {
		return err
	}
	event := api.Event{
		Type:      eventType,
		Timestamp: time.Now(),
		Metadata:  encodedMessage,
	}

	return s.broadcast(instanceID, event)
}

func (s *DevLXDServer) broadcast(instanceID int, event api.Event) error {
	s.lock.Lock()
	listeners := s.listeners
	for _, listener := range listeners {
		if !shared.StringInSlice(event.Type, listener.messageTypes) {
			continue
		}

		if listener.instanceID != instanceID {
			continue
		}

		go func(listener *DevLXDListener, event api.Event) {
			// Check that the listener still exists
			if listener == nil {
				return
			}

			// Make sure we're not done already
			if listener.IsClosed() {
				return
			}

			listener.SetWriteDeadline(time.Now().Add(5 * time.Second))
			err := listener.WriteJSON(event)
			if err != nil {
				// Remove the listener from the list
				s.lock.Lock()
				delete(s.listeners, listener.id)
				s.lock.Unlock()

				listener.Close()
			}
		}(listener, event)
	}
	s.lock.Unlock()

	return nil
}

// DevLXDListener describes a devlxd event listener.
type DevLXDListener struct {
	listenerCommon

	instanceID int
}
