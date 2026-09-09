//go:build linux || windows

package main

import (
	"fmt"
	"strings"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
)

type cpuInfo struct {
	id       int
	core     string
	kind     string
	capacity uint64
}

func selectCPUs(cpus []cpuInfo) (environment, error) {
	common.SortBy(cpus, func(info cpuInfo) int { return info.id })
	type coreClass struct {
		kind     string
		capacity uint64
	}
	groups := make(map[coreClass][]cpuInfo)
	var best []cpuInfo
	for _, info := range common.UniqBy(cpus, func(info cpuInfo) string { return info.core }) {
		class := coreClass{info.kind, info.capacity}
		group := append(groups[class], info)
		groups[class] = group
		if len(group) < 2 {
			continue
		}
		if len(best) == 0 || info.capacity > best[0].capacity || info.capacity == best[0].capacity && len(group) > len(best) {
			best = group
		}
	}
	if len(best) < 2 {
		return environment{}, E.New("CPU measurement needs at least two available physical cores of a verifiably identical class")
	}
	placement := environment{
		metric:     metricCPU,
		cpus:       common.Map(best[:len(best)/2], func(info cpuInfo) int { return info.id }),
		helperCPUs: common.Map(best[len(best)/2:], func(info cpuInfo) int { return info.id }),
	}
	placement.workers, placement.helpers = len(placement.cpus), len(placement.helperCPUs)
	placement.description = fmt.Sprintf("class %s; one thread/core; tunnel CPUs %s; helper CPUs %s",
		best[0].kind, strings.Join(F.MapToString(placement.cpus), ","), strings.Join(F.MapToString(placement.helperCPUs), ","))
	return placement, nil
}
