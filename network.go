package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strconv"
	"time"

	tun "github.com/sagernet/sing-tun"
	E "github.com/sagernet/sing/common/exceptions"
)

var errUDPPayloadIntegrity = E.New("UDP payload integrity failure")

func prepareNetwork(ctx context.Context, configuration benchmarkOptions, placement *environment) error {
	placement.address = netip.MustParsePrefix("198.18.0.1/29")
	placement.target = netip.MustParseAddr("198.18.0.3")
	if configuration.IP == 6 {
		placement.address = netip.MustParsePrefix("fd00::1/125")
		placement.target = netip.MustParseAddr("fd00::3")
	}
	routes, err := routePrefixes(ctx)
	if err != nil {
		return E.Cause(err, "inspect existing routes")
	}
	subnets := []netip.Prefix{placement.address.Masked()}
	if configuration.software == "leaf" && configuration.IP == 6 {
		subnets = append(subnets, netip.MustParsePrefix("198.18.0.0/29"))
	}
	for _, subnet := range subnets {
		for _, prefix := range routes {
			if prefix.Bits() != 0 && prefix.Overlaps(subnet) {
				return E.New("benchmark subnet ", subnet, " overlaps existing route ", prefix)
			}
		}
	}
	placement.interfaceName = tun.CalculateInterfaceName("tun-bench")
	return nil
}

func serveProbe(listener net.Listener, token string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		request := make([]byte, len(token))
		_, err = io.ReadFull(conn, request)
		if err == nil && string(request) == token {
			_, _ = conn.Write(request)
		}
		_ = conn.Close()
	}
}

func probeTunnel(ctx context.Context, address, token string) error {
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.SetDeadline(time.Now().Add(time.Second))
	if err != nil {
		return err
	}
	_, err = io.WriteString(conn, token)
	if err != nil {
		return err
	}
	response := make([]byte, len(token))
	_, err = io.ReadFull(conn, response)
	if err != nil {
		return err
	}
	if string(response) != token {
		return E.New("unexpected forwarding probe response: ", strconv.Quote(string(response)))
	}
	return nil
}

func servePacketProbe(conn *net.UDPConn, token string, payloadLength int, direction string) {
	buffer := make([]byte, 65535)
	payload := bytes.Repeat([]byte(token), (payloadLength+len(token)-1)/len(token))[:payloadLength]
	for {
		n, source, err := conn.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		if !bytes.HasPrefix(buffer[:n], []byte(token)) {
			continue
		}
		response := payload
		if direction == "upload" {
			digest := sha256.Sum256(buffer[:n])
			response = binary.BigEndian.AppendUint32([]byte(token), uint32(n))
			response = append(response, digest[:]...)
		}
		_, _ = conn.WriteToUDPAddrPort(response, source)
	}
}

func probePacketTunnel(ctx context.Context, address, token string, payloadLength int, direction string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	packetConn := conn.(*net.UDPConn)
	err = E.Errors(packetConn.SetReadBuffer(4<<20), packetConn.SetWriteBuffer(4<<20))
	if err != nil {
		return err
	}
	payload := bytes.Repeat([]byte(token), (payloadLength+len(token)-1)/len(token))[:payloadLength]
	request := payload
	if direction == "download" {
		request = []byte(token)
	}
	response := make([]byte, 65535)
	for {
		err = conn.SetDeadline(time.Now().Add(300 * time.Millisecond))
		if err != nil {
			return E.Errors(ctx.Err(), err)
		}
		_, err = conn.Write(request)
		if err != nil {
			return E.Cause(E.Errors(ctx.Err(), err), "send UDP probe")
		}
		var n int
		n, err = conn.Read(response)
		if err != nil {
			if E.IsTimeout(err) && ctx.Err() == nil {
				continue
			}
			return E.Cause(E.Errors(ctx.Err(), err), "receive UDP probe")
		}
		receivedLength := n
		if direction == "upload" {
			if n != len(token)+4+sha256.Size || !bytes.HasPrefix(response[:n], []byte(token)) {
				return E.Extend(errUDPPayloadIntegrity, ": invalid UDP probe acknowledgement")
			}
			receivedLength = int(binary.BigEndian.Uint32(response[len(token):]))
		}
		if receivedLength != len(payload) {
			return E.Extend(errUDPPayloadIntegrity, ": ", direction, " payload changed in transit: sent ", len(payload), " bytes, received ", receivedLength)
		}
		expected := payload
		actual := response[:n]
		if direction == "upload" {
			digest := sha256.Sum256(payload)
			expected = digest[:]
			actual = response[len(token)+4 : n]
		}
		if !bytes.Equal(actual, expected) {
			return E.Extend(errUDPPayloadIntegrity, ": ", direction, " payload corrupted in transit")
		}
		return nil
	}
}
