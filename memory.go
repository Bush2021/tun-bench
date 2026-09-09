package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
)

const (
	memoryConnectionStep = 250
	memoryMaxConnections = 1000
	memorySettle         = time.Second
	memoryPayloadSize    = 64
	memoryConcurrency    = 32
)

type memoryProtocol struct {
	SettleSeconds    float64 `json:"settle_seconds"`
	PayloadBytes     int     `json:"payload_bytes"`
	SetupConcurrency int     `json:"setup_concurrency"`
}

var currentMemoryProtocol = memoryProtocol{
	SettleSeconds: memorySettle.Seconds(),
	PayloadBytes:  memoryPayloadSize, SetupConcurrency: memoryConcurrency,
}

type memoryResult struct {
	ConnectionStep int            `json:"connection_step"`
	MaxConnections int            `json:"max_connections"`
	Protocol       memoryProtocol `json:"protocol"`
	Metric         string         `json:"metric"`
	Points         []memoryPoint  `json:"points"`
}

type memoryPoint struct {
	Connections    int       `json:"connections"`
	PID            int       `json:"pid"`
	Bytes          uint64    `json:"bytes"`
	At             time.Time `json:"at"`
	ReadSeconds    float64   `json:"read_seconds"`
	SetupSeconds   float64   `json:"setup_seconds"`
	ObserveSeconds float64   `json:"observe_seconds"`
}

func (m *memoryResult) growth() float64 {
	points := m.Points[1:]
	var meanConnections, meanBytes float64
	for _, point := range points {
		meanConnections += float64(point.Connections)
		meanBytes += float64(point.Bytes)
	}
	meanConnections /= float64(len(points))
	meanBytes /= float64(len(points))
	var covariance, variance float64
	for _, point := range points {
		connections := float64(point.Connections) - meanConnections
		covariance += connections * (float64(point.Bytes) - meanBytes)
		variance += connections * connections
	}
	return covariance / variance
}

func (b *benchmark) takeMemorySnapshot() (memoryPoint, error) {
	err := verifyProcesses(b.tunnels)
	if err != nil {
		return memoryPoint{}, err
	}
	pid := b.tunnels[0].command.Process.Pid
	started := time.Now()
	value, err := readMemory(pid)
	if err != nil {
		return memoryPoint{}, E.Cause(err, "read memory for PID ", pid)
	}
	elapsed := time.Since(started)
	return memoryPoint{PID: pid, Bytes: value, At: started.Add(elapsed / 2).UTC(), ReadSeconds: elapsed.Seconds()}, nil
}

func (b *benchmark) runMemory(ctx context.Context) error {
	err := prepareMemoryLimit()
	if err != nil {
		return err
	}
	load, err := startMemoryLoad(ctx, b.options.Network, net.JoinHostPort(b.options.loopback(), "0"))
	if err != nil {
		return err
	}
	b.memoryLoad = load
	b.serverPorts = []int{load.port}
	err = b.startTunnel(ctx)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(b.environment.target.String(), strconv.Itoa(load.port))
	for count := 0; count <= memoryMaxConnections; count += memoryConnectionStep {
		setupStarted := time.Now()
		if count > 0 {
			err = load.add(address, count)
			if err != nil {
				return E.Cause(err, "establish ", count, " memory connections")
			}
			err = load.confirm()
			if err != nil {
				return E.Cause(err, "confirm idle connections")
			}
		}
		setupSeconds := time.Since(setupStarted).Seconds()
		observeStarted := time.Now()
		timer := time.NewTimer(memorySettle)
		select {
		case <-load.ctx.Done():
			timer.Stop()
			return context.Cause(load.ctx)
		case <-timer.C:
		}
		point, snapshotErr := b.takeMemorySnapshot()
		if snapshotErr != nil {
			return snapshotErr
		}
		point.Connections, point.SetupSeconds = count, setupSeconds
		point.ObserveSeconds = point.At.Sub(observeStarted).Seconds()
		b.result.Memory.Points = append(b.result.Memory.Points, point)
		b.recordErr = b.matrix.saveReport(b.matrix.report, nil)
		if b.recordErr != nil {
			return E.Cause(b.recordErr, "save memory measurement")
		}
		if count > 0 {
			err = load.confirm()
		}
		err = E.Errors(err, context.Cause(load.ctx), verifyProcesses(b.tunnels))
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *memoryResult) validate(status string) error {
	if m.ConnectionStep != memoryConnectionStep || m.MaxConnections != memoryMaxConnections || m.Protocol != currentMemoryProtocol || m.Metric == "" {
		return E.New("invalid memory settings or metric")
	}
	for i, point := range m.Points {
		expected := min(i*m.ConnectionStep, m.MaxConnections)
		if point.Connections != expected || i > (m.MaxConnections-1)/m.ConnectionStep+1 || point.At.IsZero() || point.PID <= 0 {
			return E.New("invalid memory point ", i+1)
		}
		for _, seconds := range []float64{point.ReadSeconds, point.SetupSeconds, point.ObserveSeconds} {
			if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
				return E.New("invalid memory timing")
			}
		}

	}
	if status == "passed" && (len(m.Points) == 0 || m.Points[len(m.Points)-1].Connections != m.MaxConnections) {
		return E.New("incomplete memory curve")
	}
	return nil
}

type memoryLoad struct {
	ctx              context.Context
	cancel           context.CancelCauseFunc
	connectionCtx    context.Context
	closeConnections context.CancelFunc
	network          string
	port             int
	token            [16]byte
	flows            []*memoryFlow
	workers          sync.WaitGroup
}

type memoryFlow struct {
	conn     net.Conn
	payload  [memoryPayloadSize]byte
	sequence uint64
}

func startMemoryLoad(ctx context.Context, network, address string) (*memoryLoad, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	connectionCtx, closeConnections := context.WithCancel(context.WithoutCancel(ctx))
	load := &memoryLoad{
		ctx: ctx, cancel: cancel, connectionCtx: connectionCtx, closeConnections: closeConnections,
		network: network, flows: make([]*memoryFlow, 0, memoryMaxConnections),
	}
	_, _ = rand.Read(load.token[:])
	if network == "tcp" {
		listener, listenErr := net.Listen(network, address)
		if listenErr != nil {
			load.Close()
			return nil, listenErr
		}
		load.port = listener.Addr().(*net.TCPAddr).Port
		stop := context.AfterFunc(connectionCtx, func() { _ = listener.Close() })
		load.workers.Go(func() {
			defer stop()
			defer listener.Close()
			for {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					if ctx.Err() == nil {
						load.cancel(acceptErr)
					}
					return
				}
				load.workers.Go(func() {
					defer conn.Close()
					stopConnection := context.AfterFunc(connectionCtx, func() { _ = conn.Close() })
					defer stopConnection()
					tcp := conn.(*net.TCPConn)
					keepaliveErr := tcp.SetKeepAlive(false)
					if keepaliveErr != nil {
						load.cancel(keepaliveErr)
						return
					}
					var payload [memoryPayloadSize]byte
					for {
						_, readErr := io.ReadFull(conn, payload[:])
						if readErr != nil {
							return
						}
						_, writeErr := conn.Write(payload[:])
						if writeErr != nil {
							return
						}
					}
				})
			}
		})
	} else {
		listener, listenErr := net.ListenPacket(network, address)
		if listenErr != nil {
			load.Close()
			return nil, listenErr
		}
		load.port = listener.LocalAddr().(*net.UDPAddr).Port
		stop := context.AfterFunc(connectionCtx, func() { _ = listener.Close() })
		load.workers.Go(func() {
			defer stop()
			defer listener.Close()
			var payload [memoryPayloadSize + 1]byte
			for {
				n, remote, readErr := listener.ReadFrom(payload[:])
				if readErr != nil {
					if ctx.Err() == nil {
						load.cancel(readErr)
					}
					return
				}
				_, writeErr := listener.WriteTo(payload[:n], remote)
				if writeErr != nil {
					load.cancel(writeErr)
					return
				}
			}
		})
	}
	return load, nil
}

func (l *memoryLoad) Close() {
	l.cancel(nil)
	l.closeConnections()
	l.workers.Wait()
}

func (l *memoryLoad) add(address string, count int) error {
	previous := len(l.flows)
	l.flows = l.flows[:count]
	var next atomic.Int64
	next.Store(int64(previous))
	var workers sync.WaitGroup
	for range min(memoryConcurrency, count-previous) {
		workers.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= count || l.ctx.Err() != nil {
					return
				}
				dialer := net.Dialer{Timeout: 3 * time.Second, KeepAlive: -1}
				conn, err := dialer.DialContext(l.ctx, l.network, address)
				if err != nil {
					l.cancel(E.Cause(err, "dial memory flow ", index+1))
					return
				}
				flow := &memoryFlow{conn: conn}
				copy(flow.payload[:], l.token[:])
				binary.BigEndian.PutUint64(flow.payload[16:24], uint64(index))
				copy(flow.payload[32:], "tun-bench memory echo payload")
				stop := context.AfterFunc(l.connectionCtx, func() { _ = conn.Close() })
				l.workers.Go(func() {
					defer stop()
					defer conn.Close()
					<-l.connectionCtx.Done()
				})
				err = flow.exchange()
				if err != nil {
					l.cancel(E.Cause(err, "confirm memory flow ", index+1))
					return
				}
				l.flows[index] = flow
			}
		})
	}
	workers.Wait()
	return context.Cause(l.ctx)
}

func (f *memoryFlow) exchange() error {
	f.sequence++
	binary.BigEndian.PutUint64(f.payload[24:32], f.sequence)
	deadline := time.Now().Add(3 * time.Second)
	_, tcp := f.conn.(*net.TCPConn)
	var response [memoryPayloadSize + 1]byte
	for {
		attemptDeadline := deadline
		if !tcp {
			nextAttempt := time.Now().Add(250 * time.Millisecond)
			if nextAttempt.Before(deadline) {
				attemptDeadline = nextAttempt
			}
		}
		err := f.conn.SetDeadline(attemptDeadline)
		if err != nil {
			return err
		}
		_, err = f.conn.Write(f.payload[:])
		if err != nil {
			return err
		}
		for {
			var n int
			if tcp {
				n, err = io.ReadFull(f.conn, response[:memoryPayloadSize])
			} else {
				n, err = f.conn.Read(response[:])
			}
			if err != nil {
				break
			}
			if n == memoryPayloadSize && bytes.Equal(response[:n], f.payload[:]) {
				return nil
			}
			if n == memoryPayloadSize && bytes.Equal(response[:24], f.payload[:24]) && binary.BigEndian.Uint64(response[24:32]) < f.sequence {
				continue
			}
			return E.New("memory echo payload mismatch")
		}
		if tcp || !E.IsTimeout(err) || !time.Now().Before(deadline) {
			return err
		}
	}
}

func (l *memoryLoad) confirm() error {
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(memoryConcurrency, len(l.flows)) {
		workers.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= len(l.flows) || l.ctx.Err() != nil {
					return
				}
				err := l.flows[index].exchange()
				if err != nil {
					l.cancel(E.Cause(err, "check retained flow ", index+1))
					return
				}
			}
		})
	}
	workers.Wait()
	return context.Cause(l.ctx)
}
