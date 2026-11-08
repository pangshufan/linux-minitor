package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"monitor/internal/transport"
	"monitor/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var (
	errOffline = errors.New("agent is offline")
	errBusy    = errors.New("agent query queue is full")
)

type hub struct {
	protocol.UnimplementedMonitorServer
	store        *store
	plaintext    bool
	authToken    string
	offlineAfter time.Duration
	mu           sync.RWMutex
	sessions     map[string]*session
}

type session struct {
	connected time.Time
	send      chan *protocol.ServerMessage
	done      chan struct{}

	mu           sync.Mutex
	lastSeen     time.Time
	lastProgress time.Time
	sampleID     uint64
	pending      map[string]chan *protocol.QueryResult
}

func peerHost(ctx context.Context) (string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing peer")
	}
	tls, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tls.State.VerifiedChains) == 0 {
		return "", status.Error(codes.Unauthenticated, "verified client certificate required")
	}
	var hostID string
	for _, uri := range tls.State.VerifiedChains[0][0].URIs {
		id, found := strings.CutPrefix(uri.String(), transport.HostURIPrefix)
		if !found {
			continue
		}
		if hostID != "" || !transport.ValidHostID(id) {
			return "", status.Error(codes.Unauthenticated, "invalid host certificate identity")
		}
		hostID = id
	}
	if hostID == "" {
		return "", status.Error(codes.Unauthenticated, "host identity URI required")
	}
	return hostID, nil
}

func (h *hub) Connect(stream grpc.BidiStreamingServer[protocol.AgentMessage, protocol.ServerMessage]) error {
	ctx := stream.Context()
	var id string
	if h.authToken != "" {
		md, _ := metadata.FromIncomingContext(ctx)
		values := md.Get("authorization")
		want := sha256.Sum256([]byte("Bearer " + h.authToken))
		var got [32]byte
		if len(values) == 1 {
			got = sha256.Sum256([]byte(values[0]))
		}
		if len(values) != 1 || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			return status.Error(codes.Unauthenticated, "invalid Agent token")
		}
	}
	if !h.plaintext {
		var err error
		id, err = peerHost(ctx)
		if err != nil {
			return err
		}
	}
	incoming := make(chan *protocol.AgentMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case incoming <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()
	idle := time.NewTimer(h.offlineAfter)
	defer idle.Stop()
	var hello *protocol.Hello
	select {
	case msg := <-incoming:
		hello = msg.GetHello()
	case err := <-recvErr:
		return receiveStatus(err)
	case <-idle.C:
		return status.Error(codes.DeadlineExceeded, "registration timed out")
	case <-ctx.Done():
		return ctx.Err()
	}
	if h.plaintext && hello != nil && transport.ValidHostID(hello.HostId) {
		id = hello.HostId
	}
	if hello == nil || id == "" || hello.HostId != id {
		return status.Error(codes.PermissionDenied, "invalid host ID or certificate identity mismatch")
	}
	if run, err := hex.DecodeString(hello.RunId); err != nil || len(run) != 16 ||
		hello.ReportIntervalMs < 1000 || hello.ReportIntervalMs > (24*time.Hour).Milliseconds() {
		return status.Error(codes.InvalidArgument, "invalid process ID or report interval")
	}

	now := time.Now()
	s := &session{
		connected: now, lastSeen: now, lastProgress: now,
		send: make(chan *protocol.ServerMessage, 16), done: make(chan struct{}),
		pending: make(map[string]chan *protocol.QueryResult),
	}
	h.mu.Lock()
	if h.sessions[id] != nil {
		h.mu.Unlock()
		return status.Error(codes.AlreadyExists, "host already connected")
	}
	h.sessions[id] = s
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.sessions, id)
		close(s.done)
		h.mu.Unlock()
	}()
	if err := h.store.hello(ctx, hello, now); err != nil {
		log.Printf("register host %s: %v", id, err)
		return status.Error(codes.Unavailable, "storage unavailable")
	}

	sendErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-s.send:
				if err := stream.Send(msg); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	ack := &protocol.ServerMessage{Body: &protocol.ServerMessage_Ack{Ack: &protocol.Ack{}}}
	s.send <- ack
	idle.Reset(h.offlineAfter)
	for {
		select {
		case <-idle.C:
			return status.Error(codes.DeadlineExceeded, "agent heartbeat expired")
		case err := <-sendErr:
			return err
		case err := <-recvErr:
			return receiveStatus(err)
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-incoming:
			now := time.Now()
			switch body := msg.Body.(type) {
			case *protocol.AgentMessage_Heartbeat:
				if err := h.store.seen(ctx, id, now); err != nil {
					log.Printf("heartbeat for %s: %v", id, err)
					return status.Error(codes.Unavailable, "storage unavailable")
				}
				s.observe(now, body.Heartbeat.GetLatestSampleId())
				idle.Reset(h.offlineAfter)
				select {
				case s.send <- ack:
				default:
					return status.Error(codes.ResourceExhausted, "agent stopped reading responses")
				}
			case *protocol.AgentMessage_Snapshot:
				metric, _, _, err := validateSnapshot(id, body.Snapshot)
				if err != nil {
					return status.Error(codes.InvalidArgument, err.Error())
				}
				if err := h.store.save(ctx, id, hello.RunId, body.Snapshot, now); err != nil {
					log.Printf("save sample for %s: %v", id, err)
					return status.Error(codes.Unavailable, "storage unavailable")
				}
				s.observe(now, uint64(metric.SampleID))
				idle.Reset(h.offlineAfter)
			case *protocol.AgentMessage_QueryResult:
				s.resolve(body.QueryResult)
			default:
				return status.Error(codes.InvalidArgument, "unexpected stream message")
			}
		}
	}
}

func receiveStatus(err error) error {
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func (s *session) observe(now time.Time, sample uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeen = now
	if sample > s.sampleID {
		s.sampleID, s.lastProgress = sample, now
	}
}

func (s *session) resolve(result *protocol.QueryResult) {
	if result == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch := s.pending[result.RequestId]; ch != nil {
		delete(s.pending, result.RequestId)
		ch <- result
	}
}

func (h *hub) state(r *hostRecord, now time.Time) {
	r.Collecting = "unknown"
	h.mu.RLock()
	s := h.sessions[r.ID]
	h.mu.RUnlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Online = now.Sub(s.lastSeen) < h.offlineAfter
	if !r.Online {
		return
	}
	r.LastSeenMS, r.ConnectedMS = s.lastSeen.UnixMilli(), s.connected.UnixMilli()
	switch {
	case now.Sub(s.lastProgress) >= h.offlineAfter:
		r.Collecting = "stalled"
	case s.sampleID == 0:
		r.Collecting = "warming_up"
	default:
		r.Collecting = "active"
	}
}

func (h *hub) query(ctx context.Context, id string, kind protocol.Query_Kind) (*protocol.Snapshot, error) {
	h.mu.RLock()
	s := h.sessions[id]
	h.mu.RUnlock()
	if s == nil {
		return nil, errOffline
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	requestID := hex.EncodeToString(nonce[:])
	result := make(chan *protocol.QueryResult, 1)
	s.mu.Lock()
	if time.Since(s.lastSeen) >= h.offlineAfter {
		s.mu.Unlock()
		return nil, errOffline
	}
	if len(s.pending) >= 8 {
		s.mu.Unlock()
		return nil, errBusy
	}
	s.pending[requestID] = result
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, requestID)
		s.mu.Unlock()
	}()
	select {
	case <-s.done:
		return nil, errOffline
	case s.send <- &protocol.ServerMessage{Body: &protocol.ServerMessage_Query{Query: &protocol.Query{
		RequestId: requestID, Kind: kind,
	}}}:
	default:
		return nil, errBusy
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errOffline
	case reply := <-result:
		if reply.Error != "" {
			return nil, errors.New(reply.Error)
		}
		if _, _, _, err := validateSnapshot(id, reply.Snapshot); err != nil {
			return nil, err
		}
		return reply.Snapshot, nil
	}
}
