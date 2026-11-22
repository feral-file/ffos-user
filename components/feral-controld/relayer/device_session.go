package relayer

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type relaySourceKey struct{}
type relaySource struct {
	session *deviceSession
	peer    string
}
type relayPeer struct {
	sending  chan struct{}
	assembly relaywire.Assembler
}

// deviceSession owns routing for exactly one socket generation. Command replies
// carry that generation in context so reconnects never redirect an old response.
type deviceSession struct {
	conn         wrapper.WebSocketConn
	mu           sync.Mutex
	writeMu      sync.Mutex
	nextWrite    time.Time
	peers        map[string]*relayPeer
	pending      map[string]chan error
	done         chan struct{}
	once         sync.Once
	onDisconnect func(Peer)
}

func newDeviceSession(conn wrapper.WebSocketConn) *deviceSession {
	return &deviceSession{conn: conn, peers: map[string]*relayPeer{}, pending: map[string]chan error{}, done: make(chan struct{})}
}

func (s *deviceSession) close() {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		ids := make([]string, 0, len(s.peers))
		for id := range s.peers {
			ids = append(ids, id)
		}
		s.peers = make(map[string]*relayPeer)
		s.mu.Unlock()
		for _, id := range ids {
			s.disconnected(id)
		}
	})
}

func (s *deviceSession) disconnected(id string) {
	if s.onDisconnect != nil {
		s.onDisconnect(Peer{relaySource{s, id}})
	}
}

func (s *deviceSession) write(kind int, body []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// Share pacing across every destination and receipt on this socket.
	if delay := time.Until(s.nextWrite); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-s.done:
			return ErrNotConnected
		}
	}
	s.nextWrite = time.Now().Add(max(20*time.Millisecond, time.Duration(len(body))*time.Second/750000))
	if err := s.conn.SetWriteDeadline(time.Now().Add(WRITE_WAIT)); err != nil {
		return err
	}
	return s.conn.WriteMessage(kind, body)
}

func (s *deviceSession) control(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.write(websocket.TextMessage, body)
}

// receive handles transport controls separately from unchanged application JSON.
// The caller is the sole reader, including chunk assembly ownership.
func (s *deviceSession) receive(kind int, body []byte) ([]byte, string, error) {
	if kind == websocket.TextMessage {
		var event struct {
			Type         string   `json:"type"`
			ConnectionID string   `json:"connectionId"`
			Peers        []string `json:"peers"`
			ID           string   `json:"id"`
			From         string   `json:"from"`
			To           string   `json:"to"`
			Code         string   `json:"code"`
		}
		if err := json.Unmarshal(body, &event); err != nil {
			return nil, "", err
		}
		if event.Type == "peer_disconnected" {
			s.mu.Lock()
			delete(s.peers, event.ConnectionID)
			s.mu.Unlock()
			s.disconnected(event.ConnectionID)
			return nil, "", nil
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		add := func(id string) {
			if id != "" && s.peers[id] == nil && len(s.peers) < 36 {
				s.peers[id] = &relayPeer{sending: make(chan struct{}, 1)}
			}
		}
		switch event.Type {
		case "hello":
			for _, id := range event.Peers {
				add(id)
			}
		case "peer_connected":
			add(event.ConnectionID)
		case "ack":
			if ch := s.pending[event.From+":"+event.ID]; ch != nil {
				select {
				case ch <- nil:
				default:
				}
			}
		case "error":
			if ch := s.pending[event.To+":"+event.ID]; ch != nil {
				select {
				case ch <- errors.New("relay: " + event.Code):
				default:
				}
			}
		}
		return nil, "", nil
	}
	h, payload, err := relaywire.Decode(body)
	if err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	peer := s.peers[h.From]
	s.mu.Unlock()
	// Receipt credit belongs to transport, even when application data is invalid.
	if err = s.control(map[string]string{"type": "ack", "from": h.From, "id": h.ID}); err != nil {
		return nil, "", err
	}
	if peer == nil {
		return nil, "", errors.New("unknown relay source")
	}
	complete, err := peer.assembly.Add(payload, time.Now())
	if err != nil {
		return nil, "", err
	}
	return complete, h.From, nil
}

// send requires explicit source context; notifications use verified Peer bindings. Transport ACKs release bounded credit, not command success.
func (s *deviceSession) send(ctx context.Context, body []byte) error {
	if source, ok := ctx.Value(relaySourceKey{}).(relaySource); ok {
		if source.session != s {
			return errors.New("relay connection changed")
		}
		return s.sendPeer(ctx, source.peer, body)
	}
	return errors.New("v2 send requires an explicit peer")
}

func (s *deviceSession) sendPeer(parent context.Context, to string, body []byte) error {
	budget := 30 * time.Second
	if len(body) > relaywire.ChunkBytes {
		budget = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	s.mu.Lock()
	peer := s.peers[to]
	s.mu.Unlock()
	if peer == nil {
		return errors.New("relay peer disconnected")
	}
	select {
	case peer.sending <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrNotConnected
	}
	defer func() { <-peer.sending }()
	parts, err := relaywire.Split(uuid.NewString(), body)
	if err != nil {
		return err
	}
	for _, part := range parts {
		if err := s.sendFrame(ctx, to, part); err != nil {
			return err
		}
	}
	return nil
}

func (s *deviceSession) sendFrame(ctx context.Context, to string, body []byte) error {
	id := uuid.NewString()
	key := to + ":" + id
	frame, err := relaywire.Encode(relaywire.Header{Version: 2, ID: id, To: to}, body)
	if err != nil {
		return err
	}
	ack := make(chan error, 1)
	s.mu.Lock()
	s.pending[key] = ack
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }()
	if err = s.write(websocket.BinaryMessage, frame); err != nil {
		return err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrNotConnected
	case <-timer.C:
		return errors.New("relay receipt timeout")
	}
}
