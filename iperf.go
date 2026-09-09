package main

import (
	"bytes"
	"context"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"

	"golang.org/x/mod/semver"
)

const (
	iperfConnectTimeout = 10 * time.Second
	udpSocketBufferSize = 4 * 1024 * 1024
)

var errUDPNoPayload = E.New("UDP test completed without receiving payload")

type iperfSummary struct {
	Bytes         int64   `json:"bytes"`
	BitsPerSecond float64 `json:"bits_per_second"`
	Seconds       float64 `json:"seconds"`
	LostPackets   int64   `json:"lost_packets"`
	Packets       int64   `json:"packets"`
}

type iperfResult struct {
	Raw json.RawMessage `json:"-"`
	End struct {
		Sent     *iperfSummary `json:"sum_sent"`
		Received *iperfSummary `json:"sum_received"`
	} `json:"end"`
	Error string `json:"error"`
}

func checkIperf(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := commandOutput(ctx, path, "--version")
	if err != nil {
		return "", E.Cause(err, "read iperf3 version")
	}
	line, _, _ := strings.Cut(string(output), "\n")
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "iperf" || !semver.IsValid("v"+fields[1]) {
		return "", E.New("unrecognized iperf3 version: ", line)
	}
	if semver.Compare("v"+fields[1], "v3.16.0") < 0 {
		return "", E.New("iperf3 3.16 or later is required for a thread per parallel stream: ", line)
	}
	return line, nil
}

func (b *benchmark) startIperfServer(ctx context.Context) error {
	var err error
	b.server, err = startProcess(context.WithoutCancel(ctx), processOptions{
		path:            b.options.iperf,
		cpus:            b.environment.helperCPUs,
		args:            []string{"--server", "--one-off", "--bind", b.options.loopback(), "--port", strconv.Itoa(b.port), "--forceflush"},
		env:             []string{"TMPDIR=" + b.directory},
		terminateOnStop: runtime.GOOS == "windows",
	})
	if err != nil {
		return err
	}
	// iperf3 reads the control cookie synchronously before accepting another client.
	err = b.server.waitForOutput(ctx, "Server listening on")
	if err != nil {
		return E.Cause(err, "wait for iperf3 server: ", b.server.output.String())
	}
	return nil
}

func (b *benchmark) runIperfClient(ctx context.Context, duration time.Duration) (iperfResult, error) {
	ctx, cancel := context.WithTimeout(ctx, duration+15*time.Second)
	defer cancel()
	args := []string{
		"--client", b.environment.target.String(), "--port", strconv.Itoa(b.port),
		"--time", strconv.FormatInt(int64(duration/time.Second), 10),
		"--bitrate", strconv.FormatUint(b.options.Bitrate, 10),
		"--parallel", strconv.Itoa(b.options.Parallel),
		"--json", "--interval", "0",
		"--connect-timeout", strconv.FormatInt(iperfConnectTimeout.Milliseconds(), 10),
	}
	if b.options.IP == 6 {
		args = append(args, "-6")
	} else {
		args = append(args, "-4")
	}
	if b.options.Network == "udp" {
		args = append(args, "--udp", "--length", strconv.Itoa(b.options.udpPayloadLength()), "--window", strconv.Itoa(udpSocketBufferSize))
	}
	if b.options.Direction == "download" {
		args = append(args, "--reverse")
	}
	var output bytes.Buffer
	client, err := startProcess(ctx, processOptions{
		path:            b.options.iperf,
		cpus:            b.environment.helperCPUs,
		args:            args,
		env:             []string{"TMPDIR=" + b.directory},
		stdout:          &output,
		terminateOnStop: runtime.GOOS == "windows",
	})
	if err != nil {
		return iperfResult{}, err
	}
	defer client.stop()
	<-client.done
	err = ctx.Err()
	if err != nil {
		return iperfResult{}, commandError(client.command, E.Errors(err, client.err), &client.output)
	}
	if client.err != nil {
		return iperfResult{}, client.err
	}
	var result iperfResult
	err = json.Unmarshal(output.Bytes(), &result)
	if err != nil {
		return result, commandError(client.command, E.Cause(err, "decode iperf3 result"), &client.output)
	}
	if result.Error != "" {
		return result, commandError(client.command, E.New("iperf3: ", result.Error), &client.output)
	}
	if result.End.Received == nil || result.End.Received.Bytes < 0 || result.End.Received.Seconds <= 0 {
		return result, commandError(client.command, E.New("iperf3 did not report received payload"), &client.output)
	}
	if result.End.Received.Bytes == 0 && (b.options.Network != "udp" || result.End.Sent == nil || result.End.Sent.Bytes <= 0) {
		return result, commandError(client.command, E.New("iperf3 did not report received payload"), &client.output)
	}
	select {
	case <-b.server.done:
		if b.server.err != nil {
			return result, b.server.err
		}
	case <-ctx.Done():
		return result, commandError(b.server.command, E.Cause(ctx.Err(), "wait for one-off iperf3 server"), &b.server.output)
	}
	result.Raw = output.Bytes()
	return result, nil
}

func (b *benchmark) runThroughput(ctx context.Context) error {
	var reservedPorts []net.Listener
	var reservedPacketPorts []net.PacketConn
	defer func() {
		for _, listener := range reservedPorts {
			_ = listener.Close()
		}
		for _, listener := range reservedPacketPorts {
			_ = listener.Close()
		}
	}()
	for range 2 {
		listener, err := net.Listen("tcp", net.JoinHostPort(b.options.loopback(), "0"))
		if err != nil {
			return E.Cause(err, "reserve measurement port")
		}
		reservedPorts = append(reservedPorts, listener)
		if b.options.Network == "udp" {
			packetListener, listenErr := net.ListenPacket("udp", listener.Addr().String())
			if listenErr != nil {
				return E.Cause(listenErr, "reserve UDP measurement port")
			}
			reservedPacketPorts = append(reservedPacketPorts, packetListener)
		}
		b.serverPorts = append(b.serverPorts, listener.Addr().(*net.TCPAddr).Port)
	}
	b.port = b.serverPorts[0]
	err := reservedPorts[0].Close()
	if err != nil {
		return E.Cause(err, "release warmup port")
	}
	err = b.startIperfServer(ctx)
	if err != nil {
		return err
	}
	err = b.startTunnel(ctx)
	if err != nil {
		return err
	}
	if b.options.Network == "udp" {
		err = reservedPacketPorts[0].Close()
		if err != nil {
			return E.Cause(err, "release UDP warmup port")
		}
	}
	_, err = b.runIperfClient(ctx, benchmarkWarmup)
	if err != nil {
		return E.Cause(err, "warm up")
	}
	b.port = b.serverPorts[1]
	err = reservedPorts[1].Close()
	if err != nil {
		return E.Cause(err, "release measurement port")
	}
	err = b.startIperfServer(ctx)
	if err != nil {
		return err
	}
	if b.options.Network == "udp" {
		err = reservedPacketPorts[1].Close()
		if err != nil {
			return E.Cause(err, "release UDP measurement port")
		}
	}
	err = E.Errors(b.server.verify(), verifyProcesses(b.tunnels))
	if err != nil {
		return err
	}
	before, err := b.takeSnapshot()
	if err != nil {
		return err
	}
	result, err := b.runIperfClient(ctx, benchmarkDuration)
	if err != nil {
		return E.Cause(err, "run measurement")
	}
	after, err := b.takeSnapshot()
	if err != nil {
		return err
	}
	err = verifyProcesses(b.tunnels)
	if err != nil {
		return err
	}
	if result.End.Received.Bytes == 0 {
		return E.Extend(errUDPNoPayload, ":\n", string(result.Raw))
	}
	measured, err := calculateMeasurement(before, after, *result.End.Received, b.environment.metric)
	if err != nil {
		return err
	}
	if result.End.Sent != nil {
		measured.SentBits = float64(result.End.Sent.Bytes) * 8
	}
	measured.Iperf = result.Raw
	b.result.Sample = &measured
	b.recordErr = b.matrix.saveReport(b.matrix.report, nil)
	if b.recordErr != nil {
		return E.Cause(b.recordErr, "save measurement")
	}
	return nil
}

type resourceMetric string

const (
	metricEnergy resourceMetric = "energy"
	metricCPU    resourceMetric = "cpu"
)

func (b *benchmark) takeSnapshot() (resourceSnapshot, error) {
	started := time.Now()
	err := verifyProcesses(b.tunnels)
	if err != nil {
		return resourceSnapshot{}, err
	}
	var total resourceCounters
	for _, child := range b.tunnels {
		pid := child.command.Process.Pid
		reading, readErr := readCounters(pid)
		if readErr != nil {
			return resourceSnapshot{}, E.Cause(readErr, "read tunnel counters for PID ", pid)
		}
		total.CPUNanoseconds += reading.CPUNanoseconds
		total.EnergyNanojoules += reading.EnergyNanojoules
	}
	return resourceSnapshot{At: started.Add(time.Since(started) / 2), Tunnel: total}, nil
}

func calculateMeasurement(before, after resourceSnapshot, received iperfSummary, metric resourceMetric) (sampleResult, error) {
	result := sampleResult{
		Seconds: after.At.Sub(before.At).Seconds(), Bits: float64(received.Bytes) * 8,
		Lost: received.LostPackets, Packets: received.Packets, Before: before, After: after,
	}
	result.Before.At, result.After.At = before.At.UTC(), after.At.UTC()
	if result.Seconds <= 0 || result.Bits <= 0 {
		return result, E.New("no received payload or invalid measurement interval")
	}
	first, last := before.Tunnel.CPUNanoseconds, after.Tunnel.CPUNanoseconds
	if metric == metricEnergy {
		first, last = before.Tunnel.EnergyNanojoules, after.Tunnel.EnergyNanojoules
	}
	if last <= first {
		return result, E.New(string(metric), " counter did not advance; cannot produce a comparable result")
	}
	result.Tunnel = float64(last-first) / 1e9
	return result, nil
}
