package main

import (
	"bytes"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"time"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
)

type resultReport struct {
	StartedAt    time.Time           `json:"started_at"`
	FinishedAt   *time.Time          `json:"finished_at,omitempty"`
	Environments []environmentResult `json:"environments"`
	Error        string              `json:"error,omitempty"`
}

type environmentResult struct {
	Name          string                   `json:"name"`
	Configuration environmentConfiguration `json:"configuration"`
	Report        *benchmarkReport         `json:"report,omitempty"`
	Error         string                   `json:"error,omitempty"`
}

type benchmarkReport struct {
	StartedAt       time.Time                              `json:"started_at"`
	FinishedAt      *time.Time                             `json:"finished_at,omitempty"`
	Environment     resultEnvironment                      `json:"environment"`
	Protocol        resultProtocol                         `json:"protocol"`
	Iperf           implementationConfiguration            `json:"iperf3,omitzero"`
	Implementations map[string]implementationConfiguration `json:"implementations"`
	Cases           []caseResult                           `json:"cases"`
	Error           string                                 `json:"error,omitempty"`
}

type resultEnvironment struct {
	OS            string         `json:"os"`
	Arch          string         `json:"arch"`
	Hostname      string         `json:"hostname,omitempty"`
	Metric        resourceMetric `json:"metric"`
	Description   string         `json:"description,omitempty"`
	TunnelCPUs    []int          `json:"tunnel_cpus,omitempty"`
	HelperCPUs    []int          `json:"helper_cpus,omitempty"`
	TunnelWorkers int            `json:"tunnel_workers"`
	HelperWorkers int            `json:"helper_workers"`
}

const benchmarkForwarding = "direct"

type resultProtocol struct {
	Forwarding      string  `json:"forwarding,omitempty"`
	UDPSocketBuffer int     `json:"udp_socket_buffer,omitempty"`
	DurationSeconds float64 `json:"duration_seconds"`
	WarmupSeconds   float64 `json:"warmup_seconds"`
}

var currentProtocol = resultProtocol{
	Forwarding: benchmarkForwarding, UDPSocketBuffer: udpSocketBufferSize,
	DurationSeconds: benchmarkDuration.Seconds(), WarmupSeconds: benchmarkWarmup.Seconds(),
}

type caseResult struct {
	caseConfiguration
	Queues int           `json:"queues"`
	Status string        `json:"status"`
	Sample *sampleResult `json:"sample,omitempty"`
	Memory *memoryResult `json:"memory,omitempty"`
	Bug    *caseBug      `json:"bug,omitempty"`
	Error  string        `json:"error,omitempty"`
}

type caseBug struct {
	Type   string `json:"type"`
	Detail string `json:"detail"`
}

type sampleResult struct {
	Seconds  float64          `json:"seconds"`
	Bits     float64          `json:"bits"`
	SentBits float64          `json:"sent_bits,omitempty"`
	Tunnel   float64          `json:"tunnel"`
	Lost     int64            `json:"lost_packets"`
	Packets  int64            `json:"packets"`
	Before   resourceSnapshot `json:"before"`
	After    resourceSnapshot `json:"after"`
	Iperf    json.RawMessage  `json:"iperf3"`
}

type resourceSnapshot struct {
	At     time.Time        `json:"at"`
	Tunnel resourceCounters `json:"tunnel"`
}

type resourceCounters struct {
	CPUNanoseconds   uint64 `json:"cpu_nanoseconds"`
	EnergyNanojoules uint64 `json:"energy_nanojoules"`
}

func (r *resultReport) save(destination string) error {
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(r)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".tun-bench-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	err = file.Chmod(0o644)
	if err != nil {
		return err
	}
	_, err = file.Write(content.Bytes())
	if err != nil {
		return err
	}
	err = file.Sync()
	if err != nil {
		return err
	}
	err = file.Close()
	if err != nil {
		return err
	}
	return replaceResult(file.Name(), destination)
}

func readReport(input io.Reader) (*resultReport, error) {
	var report resultReport
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&report)
	if err != nil {
		return nil, E.Cause(err, "read results")
	}
	var trailing any
	err = decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			err = E.New("expected one JSON document")
		}
		return nil, err
	}
	err = report.validate()
	return &report, err
}

func (r *resultReport) validate() error {
	seen := make(map[string]bool)
	for _, entry := range r.Environments {
		if entry.Name == "" || seen[entry.Name] {
			return E.New("invalid or duplicate result environment: ", entry.Name)
		}
		seen[entry.Name] = true
		if entry.Report != nil {
			err := entry.Report.validate()
			if err != nil {
				return E.Cause(err, "environment ", entry.Name)
			}
		}
	}
	return nil
}

func (r *benchmarkReport) validate() error {
	if r.Protocol != currentProtocol {
		return E.New("result measurement protocol differs; use another output path or --overwrite")
	}
	if r.Environment.Metric != metricCPU && r.Environment.Metric != metricEnergy {
		return E.New("unsupported resource metric: ", r.Environment.Metric)
	}
	for i, recorded := range r.Cases {
		_, loaded := r.Implementations[recorded.Implementation]
		if !loaded {
			return E.New("case ", i+1, ": unknown implementation ", recorded.Implementation)
		}
		switch recorded.Status {
		case "pending", "running", "failed", "canceled":
		case "bug":
			if recorded.Bug == nil || recorded.Bug.Type == "" || recorded.Bug.Detail == "" {
				return E.New("case ", i+1, ": missing bug details")
			}
		case "passed":
			if !recorded.measured() {
				return E.New("case ", i+1, ": missing measurements")
			}
		default:
			return E.New("case ", i+1, ": unsupported status ", recorded.Status)
		}
		if recorded.Type == "memory" {
			if recorded.Memory == nil || recorded.Sample != nil {
				return E.New("case ", i+1, ": missing memory result or mixed measurement types")
			}
			err := recorded.Memory.validate(recorded.Status)
			if err != nil {
				return E.Cause(err, "case ", i+1)
			}
			continue
		}
		if recorded.Type != "throughput" || recorded.Memory != nil {
			return E.New("case ", i+1, ": invalid measurement type")
		}
		sample := recorded.Sample
		if sample != nil {
			for _, value := range []float64{sample.Seconds, sample.Bits, sample.Tunnel} {
				if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
					return E.New("case ", i+1, ": invalid measurement")
				}
			}
		}
	}
	return nil
}

func (c caseResult) measured() bool {
	return c.Sample != nil || c.Memory != nil && len(c.Memory.Points) > 0
}

func (c caseResult) matches(options benchmarkOptions) bool {
	return c.caseConfiguration == options.caseConfiguration && c.Queues == options.queues
}
