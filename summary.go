package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sagernet/sing/common"
)

const minimumEfficiencyBitrate = 5e6

func (o caseConfiguration) label() string {
	label := o.Implementation
	if o.Stack != "" {
		label += " / " + o.Stack
	}
	return label
}

func (r *resultReport) writeSummary(output io.Writer) error {
	for _, entry := range r.Environments {
		_, err := fmt.Fprintf(output, "\nEnvironment: %s (%s/%s, %s)\n", entry.Name, entry.Configuration.OS, entry.Configuration.Arch, entry.Configuration.Type)
		if err != nil {
			return err
		}
		if entry.Error != "" {
			_, err = fmt.Fprintln(output, "Error:", entry.Error)
			if err != nil {
				return err
			}
		}
		if entry.Report != nil {
			err = entry.Report.writeSummary(output)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *benchmarkReport) writeSummary(output io.Writer) error {
	if len(r.Cases) == 0 {
		return nil
	}
	var content strings.Builder
	content.WriteString("Results\n=======\n")
	content.WriteString("Forwarding: direct")
	relayed := common.Uniq(common.Map(common.Filter(r.Cases, func(recorded caseResult) bool {
		implementation := r.Implementations[recorded.Implementation]
		return implementation.Type != "sing-box" && implementation.Type != "leaf"
	}), func(recorded caseResult) string { return recorded.Implementation }))
	if len(relayed) > 0 {
		fmt.Fprintf(&content, "; SOCKS5 relay for %s", strings.Join(relayed, ", "))
	}
	content.WriteByte('\n')

	type workload struct {
		ip              int
		network         string
		direction       string
		measurementType string
		mtu             int
		parallel        int
		bitrate         uint64
	}
	for _, matrix := range common.Uniq(common.Map(r.Cases, func(recorded caseResult) string { return recorded.Matrix })) {
		fmt.Fprintf(&content, "\n%s\n%s\n", matrix, strings.Repeat("-", len(matrix)))
		var groups [][]int
		groupIndexes := make(map[workload]int)
		for i, options := range r.Cases {
			if options.Matrix != matrix {
				continue
			}
			key := workload{options.IP, options.Network, options.Direction, options.Type, options.MTU, options.Parallel, options.Bitrate}
			index, loaded := groupIndexes[key]
			if !loaded {
				index = len(groups)
				groupIndexes[key] = index
				groups = append(groups, nil)
			}
			groups[index] = append(groups[index], i)
		}
		for _, indices := range groups {
			options := r.Cases[indices[0]]
			if options.Type == "memory" {
				writeMemorySummary(&content, r, indices)
				continue
			}
			slices.SortStableFunc(indices, func(a, b int) int {
				if r.Cases[a].Status != "passed" {
					if r.Cases[b].Status == "passed" {
						return 1
					}
					return 0
				}
				if r.Cases[b].Status != "passed" {
					return -1
				}
				first, second := r.Cases[a].Sample, r.Cases[b].Sample
				firstBitrate, secondBitrate := first.Bits/first.Seconds, second.Bits/second.Seconds
				if options.Bitrate == 0 || firstBitrate < minimumEfficiencyBitrate || secondBitrate < minimumEfficiencyBitrate {
					return cmp.Compare(secondBitrate, firstBitrate)
				}
				return cmp.Compare(first.Tunnel/first.Bits, second.Tunnel/second.Bits)
			})
			fmt.Fprintf(&content, "\n  IPv%d %s %s\n", options.IP, strings.ToUpper(options.Network), options.Direction)
			fmt.Fprintf(&content, "  MTU %d", options.MTU)
			if options.Parallel > 1 {
				fmt.Fprintf(&content, ", parallel %d", options.Parallel)
			}
			if options.Bitrate == 0 {
				content.WriteString(", unlimited\n\n")
			} else {
				fmt.Fprintf(&content, ", bitrate/stream %.2f Mbit/s\n\n", float64(options.Bitrate)/1e6)
			}
			header := []string{"Implementation", "Gbit/s"}
			if options.Network == "udp" {
				header = append(header, "Send Gbit/s")
			}
			switch r.Environment.Metric {
			case metricEnergy:
				header = append(header, "Tunnel W", "J/Gbit")
			case metricCPU:
				header = append(header, "Tunnel CPU", "CPU %/(Gbit/s)")
			}
			if options.Network == "udp" {
				header = append(header, "Loss")
			}
			rows := append([][]string{header}, common.Map(indices, func(index int) []string {
				current := r.Cases[index]
				row := []string{current.label()}
				if current.Status != "passed" {
					status := current.Status
					if current.Bug != nil && status == "bug" {
						status += " (" + current.Bug.Type + ")"
					}
					row = append(row, status)
					for len(row) < len(header) {
						row = append(row, "-")
					}
					return row
				}
				measured := current.Sample
				bitrate := measured.Bits / measured.Seconds
				row = append(row, fmt.Sprintf("%.2f", bitrate/1e9))
				if current.Network == "udp" {
					sendRate := "n/a"
					if measured.SentBits > 0 {
						sendRate = fmt.Sprintf("%.2f", measured.SentBits/measured.Seconds/1e9)
					}
					row = append(row, sendRate)
				}
				cost := "-"
				switch r.Environment.Metric {
				case metricEnergy:
					if bitrate >= minimumEfficiencyBitrate {
						cost = fmt.Sprintf("%.2f", measured.Tunnel/(measured.Bits/1e9))
					}
					row = append(row, fmt.Sprintf("%.2f", measured.Tunnel/measured.Seconds), cost)
				case metricCPU:
					if bitrate >= minimumEfficiencyBitrate {
						cost = fmt.Sprintf("%.2f%%", measured.Tunnel/(measured.Bits/1e9)*100)
					}
					row = append(row, fmt.Sprintf("%.2f%%", measured.Tunnel/measured.Seconds*100), cost)
				}
				if current.Network == "udp" {
					loss := "n/a"
					if measured.Packets > 0 {
						loss = fmt.Sprintf("%.2f%%", float64(measured.Lost)/float64(measured.Packets)*100)
					}
					row = append(row, loss)
				}
				return row
			})...)
			writeSummaryTable(&content, rows)
		}
	}
	_, err := io.WriteString(output, content.String())
	return err
}

func writeMemorySummary(content *strings.Builder, r *benchmarkReport, indices []int) {
	growths := make(map[int]float64)
	for _, index := range indices {
		current := r.Cases[index]
		if current.Status != "passed" {
			continue
		}
		growths[index] = current.Memory.growth()
	}
	slices.SortStableFunc(indices, func(a, b int) int {
		first, firstLoaded := growths[a]
		second, secondLoaded := growths[b]
		switch {
		case firstLoaded && secondLoaded:
			return cmp.Compare(first, second)
		case firstLoaded:
			return -1
		case secondLoaded:
			return 1
		default:
			return 0
		}
	})
	options := r.Cases[indices[0]]
	fmt.Fprintf(content, "\n  IPv%d %s idle\n  MTU %d\n\n", options.IP, strings.ToUpper(options.Network), options.MTU)
	rows := append([][]string{{"Implementation", "MiB/100 conn"}}, common.Map(indices, func(index int) []string {
		current := r.Cases[index]
		if current.Status != "passed" {
			return []string{current.label(), current.Status}
		}
		growth, loaded := growths[index]
		if !loaded {
			return []string{current.label(), "-"}
		}
		return []string{current.label(), fmt.Sprintf("%.2f", growth*100/(1<<20))}
	})...)
	writeSummaryTable(content, rows)
}

func writeSummaryTable(output *strings.Builder, rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, value := range row {
			widths[i] = max(widths[i], len(value))
		}
	}
	for i, row := range rows {
		fmt.Fprintf(output, "  %-*s", widths[0], row[0])
		for j, value := range row[1:] {
			fmt.Fprintf(output, "  %*s", widths[j+1], value)
		}
		output.WriteByte('\n')
		if i == 0 {
			output.WriteString("  ")
			for j, width := range widths {
				if j > 0 {
					output.WriteString("  ")
				}
				output.WriteString(strings.Repeat("-", width))
			}
			output.WriteByte('\n')
		}
	}
}
