package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	random "math/rand/v2"
	"sync/atomic"
	"time"

	"monitor/internal/transport"
	"monitor/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func (a *agent) connect(ctx context.Context, c config, tlsConfig *tls.Config) error {
	var nonce [16]byte
	rand.Read(nonce[:])
	runID := hex.EncodeToString(nonce[:])
	var creds credentials.TransportCredentials = insecure.NewCredentials()
	if tlsConfig != nil {
		creds = credentials.NewTLS(tlsConfig)
	}
	conn, err := grpc.NewClient(c.server, grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(transport.MaxMessageBytes), grpc.MaxCallSendMsgSize(transport.MaxMessageBytes)))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := protocol.NewMonitorClient(conn)
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := a.stream(ctx, c, client, runID)
		if ctx.Err() != nil {
			break
		}
		log.Printf("monitor stream disconnected: %v", err)
		if time.Since(start) > 30*time.Second {
			delay = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay/2 + random.N(delay/2)):
			delay = min(delay*2, 30*time.Second)
		}
	}
	return nil
}

func (a *agent) stream(parent context.Context, c config, client protocol.MonitorClient, runID string) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var lastAck atomic.Int64
	start := time.Now()
	go periodically(ctx, c.heartbeat, false, func() {
		if time.Since(start)-time.Duration(lastAck.Load()) > 3*c.heartbeat {
			cancel()
		}
	})
	if c.authToken != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.authToken)
	}
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Hello{Hello: &protocol.Hello{
		HostId: a.hostID, RunId: runID, ReportIntervalMs: c.push.Milliseconds(),
	}}}); err != nil {
		return err
	}

	incoming := make(chan *protocol.Query)
	received := make(chan error, 1)
	go func() {
		defer close(incoming)
		for {
			msg, err := stream.Recv()
			if err != nil {
				received <- err
				return
			}
			if msg.GetAck() != nil {
				lastAck.Store(int64(time.Since(start)))
			}
			if query := msg.GetQuery(); query != nil {
				select {
				case incoming <- query:
				case <-ctx.Done():
					received <- ctx.Err()
					return
				}
			}
		}
	}()
	heartbeat, push := time.NewTicker(c.heartbeat), time.NewTicker(c.push)
	defer heartbeat.Stop()
	defer push.Stop()

	var lastInventory *inventorySnapshot
	sendSnapshot := func() error {
		inventory := a.inventory.Load()
		snapshot, err := a.snapshot(true, inventory, inventory != lastInventory)
		if err != nil {
			return err
		}
		if len(snapshot.MetricsJson)+len(snapshot.InventoryJson) == 0 {
			return nil
		}
		if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Snapshot{Snapshot: snapshot}}); err != nil {
			return err
		}
		lastInventory = inventory
		return nil
	}
	if err := sendSnapshot(); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case query, ok := <-incoming:
			if !ok {
				return <-received
			}
			if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_QueryResult{QueryResult: a.query(query)}}); err != nil {
				return err
			}
		case <-heartbeat.C:
			var id uint64
			if snapshot := a.metrics.Load(); snapshot != nil {
				id = snapshot.SampleID
			}
			if err := stream.Send(&protocol.AgentMessage{Body: &protocol.AgentMessage_Heartbeat{Heartbeat: &protocol.Heartbeat{LatestSampleId: id}}}); err != nil {
				return err
			}
		case <-push.C:
			if err := sendSnapshot(); err != nil {
				return err
			}
		}
	}
}

func (a *agent) query(query *protocol.Query) *protocol.QueryResult {
	result := &protocol.QueryResult{RequestId: query.RequestId}
	var err error
	switch query.Kind {
	case protocol.Query_KIND_LATEST:
		result.Snapshot, err = a.snapshot(true, nil, false)
		if err == nil && len(result.Snapshot.MetricsJson) == 0 {
			err = errors.New("metrics are warming up")
		}
	case protocol.Query_KIND_INVENTORY:
		result.Snapshot, err = a.snapshot(false, a.inventory.Load(), true)
		if err == nil && len(result.Snapshot.InventoryJson) == 0 {
			err = errors.New("inventory is warming up")
		}
	default:
		err = errors.New("unsupported query")
	}
	if err != nil {
		result.Error, result.Snapshot = err.Error(), nil
	}
	return result
}

func (a *agent) snapshot(latest bool, inventory *inventorySnapshot, includeInventory bool) (*protocol.Snapshot, error) {
	s := new(protocol.Snapshot)
	var err error
	if latest {
		if metrics := a.metrics.Load(); metrics != nil {
			s.MetricsJson, err = json.Marshal(metrics)
			if err != nil {
				return nil, err
			}
		}
		if fs := a.filesystems.Load(); fs != nil {
			s.FilesystemsJson, err = json.Marshal(fs)
			if err != nil {
				return nil, err
			}
		}
	}
	if includeInventory && inventory != nil {
		s.InventoryJson, err = json.Marshal(inventory)
	}
	return s, err
}
