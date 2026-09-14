package main

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/byteformats"
	E "github.com/sagernet/sing/common/exceptions"
)

const (
	benchmarkDuration = 5 * time.Second
	benchmarkWarmup   = time.Second
)

type caseConfiguration struct {
	Type           string `json:"type"`
	Matrix         string `json:"matrix"`
	Implementation string `json:"implementation"`
	Stack          string `json:"stack,omitempty"`
	IP             int    `json:"ip"`
	Network        string `json:"network"`
	Direction      string `json:"direction,omitempty"`
	Parallel       int    `json:"parallel"`
	MTU            int    `json:"mtu"`
	Bitrate        uint64 `json:"bitrate"`
}

type benchmarkOptions struct {
	caseConfiguration
	environmentName   string
	operatingSystem   string
	sourcePackage     string
	software          string
	executable        string
	version           string
	iperf             string
	iperfArchitecture string
	relayExecutable   string
	relay             bool
	queues            int
}

func (o benchmarkOptions) needsRelay() bool {
	return o.relay || slices.Contains([]string{"hev-socks5-tunnel", "xjasonlyu-tun2socks", "go-tun2socks", "mihomo"}, o.software)
}

func (o caseConfiguration) validate() error {
	if o.IP != 4 && o.IP != 6 {
		return E.New("ip must be 4 or 6")
	}
	if o.Network != "tcp" && o.Network != "udp" {
		return E.New("network must be tcp or udp")
	}
	if o.Type == "throughput" && o.Direction != "upload" && o.Direction != "download" {
		return E.New("direction must be upload or download")
	}
	if o.Parallel < 1 {
		return E.New("parallel must be positive")
	}
	if o.Parallel > 128 {
		return E.New("iperf3 supports at most 128 parallel streams")
	}
	minimumMTU := 576
	if o.IP == 6 {
		minimumMTU = 1280
	}
	if o.MTU < minimumMTU || o.MTU > 65535 {
		return E.New("MTU must be between ", minimumMTU, " and 65535")
	}
	return nil
}

func (o benchmarkOptions) multiQueue() bool {
	return o.Type != "memory" && o.Parallel > 1 && o.operatingSystem == "linux" && (o.software == "hev-socks5-tunnel" || o.software == "sing-box" && (o.Stack == "" || o.Stack == "go"))
}

func (o *benchmarkOptions) resolveQueues(placement environment) {
	o.queues = 1
	if o.multiQueue() {
		o.queues = placement.workers
	}
}

func (o benchmarkOptions) loopback() string {
	if o.IP == 6 || o.operatingSystem == "darwin" {
		return "::1"
	}
	return "127.0.0.1"
}

func (o benchmarkOptions) udpPayloadLength() int {
	const maximumIPv4UDPPayload = 65507
	if o.IP == 6 {
		return min(o.MTU-48, maximumIPv4UDPPayload)
	}
	return min(o.MTU-28, maximumIPv4UDPPayload)
}

func (o benchmarkOptions) String() string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s / %s / %s", o.environmentName, o.Matrix, o.Implementation)
	if o.version != "" {
		fmt.Fprintf(&text, " %s", o.version)
	}
	if o.Stack != "" {
		fmt.Fprintf(&text, "; stack %s", o.Stack)
	}
	fmt.Fprintf(&text, "; IPv%d %s", o.IP, strings.ToUpper(o.Network))
	if o.Direction != "" {
		fmt.Fprintf(&text, " %s", o.Direction)
	}
	fmt.Fprintf(&text, "; MTU %d", o.MTU)
	if o.Type == "memory" {
		fmt.Fprintf(&text, "; memory idle; connections 0..%d step %d", memoryMaxConnections, memoryConnectionStep)
	}
	if o.Parallel > 1 {
		fmt.Fprintf(&text, "; parallel %d", o.Parallel)
	}
	if o.Bitrate != 0 {
		fmt.Fprintf(&text, "; bitrate/stream %s", strings.ReplaceAll(byteformats.FormatBytes(o.Bitrate), "B", "bps"))
	}
	return text.String()
}

type benchmarkLogField struct {
	name  string
	value any
	text  string
}

func (o benchmarkOptions) logFields() []benchmarkLogField {
	stack := o.Stack
	if stack == "" {
		stack = "default"
	}
	fields := []benchmarkLogField{
		{"environment", o.environmentName, o.environmentName},
		{"matrix", o.Matrix, o.Matrix},
		{"implementation", o.Implementation, o.Implementation},
		{"version", o.version, o.version},
		{"stack", o.Stack, "stack " + stack},
		{"type", o.Type, o.Type},
		{"ip", o.IP, fmt.Sprintf("IPv%d", o.IP)},
		{"network", o.Network, strings.ToUpper(o.Network)},
		{"mtu", o.MTU, fmt.Sprintf("MTU %d", o.MTU)},
	}
	if o.Type != "memory" {
		bitrate := "unlimited"
		if o.Bitrate != 0 {
			bitrate = strings.ReplaceAll(byteformats.FormatBytes(o.Bitrate), "B", "bps")
		}
		fields = append(fields,
			benchmarkLogField{"direction", o.Direction, o.Direction},
			benchmarkLogField{"parallel", o.Parallel, fmt.Sprintf("parallel %d", o.Parallel)},
			benchmarkLogField{"bitrate", o.Bitrate, "bitrate/stream " + bitrate},
		)
	}
	return fields
}

func benchmarkLog(cases []benchmarkOptions) func(benchmarkOptions) string {
	baseline := make(map[string]any)
	varying := make(map[string]bool)
	for _, options := range cases {
		for _, field := range options.logFields() {
			value, loaded := baseline[field.name]
			if !loaded {
				baseline[field.name] = field.value
			} else if field.value != value {
				varying[field.name] = true
			}
		}
	}
	return func(options benchmarkOptions) string {
		fields := common.Filter(options.logFields(), func(field benchmarkLogField) bool {
			return varying[field.name] && field.text != ""
		})
		return strings.Join(common.Map(fields, func(field benchmarkLogField) string { return field.text }), "; ")
	}
}
