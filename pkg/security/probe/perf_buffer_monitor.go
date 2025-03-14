// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// +build linux

package probe

import (
	"fmt"
	"sync/atomic"

	"github.com/DataDog/datadog-go/statsd"
	manager "github.com/DataDog/ebpf-manager"
	lib "github.com/cilium/ebpf"
	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/ebpf/probes"
	"github.com/DataDog/datadog-agent/pkg/security/metrics"
	"github.com/DataDog/datadog-agent/pkg/security/model"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	"github.com/DataDog/datadog-agent/pkg/util/log"
)

// PerfMapStats contains the collected metrics for one event and one cpu in a perf buffer statistics map
type PerfMapStats struct {
	Bytes uint64
	Count uint64
	Lost  uint64
}

// UnmarshalBinary parses a map entry and populates the current PerfMapStats instance
func (s *PerfMapStats) UnmarshalBinary(data []byte) error {
	if len(data) < 24 {
		return model.ErrNotEnoughData
	}
	s.Bytes = model.ByteOrder.Uint64(data[0:8])
	s.Count = model.ByteOrder.Uint64(data[8:16])
	s.Lost = model.ByteOrder.Uint64(data[16:24])
	return nil
}

// PerfBufferMonitor holds statistics about the number of lost and received events
//nolint:structcheck,unused
type PerfBufferMonitor struct {
	// probe is a pointer to the Probe
	probe *Probe
	// statsdClient is a pointer to the statsdClient used to report the metrics of the perf buffer monitor
	statsdClient *statsd.Client
	// numCPU holds the current count of CPU
	numCPU int
	// perfBufferStatsMaps holds the pointers to the statistics kernel maps
	perfBufferStatsMaps map[string]*lib.Map
	// perfBufferSize holds the size of each perf buffer, indexed by the name of the perf buffer
	perfBufferSize map[string]float64

	// perfBufferMapNameToStatsMapsName maps a perf buffer to its statistics maps
	perfBufferMapNameToStatsMapsName map[string]string
	// statsMapsNamePerfBufferMapName maps a statistic map to its perf buffer
	statsMapsNameToPerfBufferMapName map[string]string

	// stats holds the collected user space metrics
	stats map[string][][model.MaxEventType]PerfMapStats
	// kernelStats holds the aggregated kernel space metrics
	kernelStats map[string][][model.MaxEventType]PerfMapStats
	// readLostEvents is the count of lost events, collected by reading the perf buffer
	readLostEvents map[string][]uint64
	// sortingErrorStats holds the count of events that indicate that at least 1 event is miss ordered
	sortingErrorStats map[string][model.MaxEventType]*int64

	// lastTimestamp is used to track the timestamp of the last event retrieved from the perf map
	lastTimestamp uint64
	// shouldBumpGeneration is used to track if the dentry cache generations should be bumped
	shouldBumpGeneration uint64
}

// NewPerfBufferMonitor instantiates a new event statistics counter
func NewPerfBufferMonitor(p *Probe, client *statsd.Client) (*PerfBufferMonitor, error) {
	pbm := PerfBufferMonitor{
		probe:               p,
		statsdClient:        client,
		perfBufferStatsMaps: make(map[string]*lib.Map),
		perfBufferSize:      make(map[string]float64),

		perfBufferMapNameToStatsMapsName: probes.GetPerfBufferStatisticsMaps(),
		statsMapsNameToPerfBufferMapName: make(map[string]string),

		stats:             make(map[string][][model.MaxEventType]PerfMapStats),
		kernelStats:       make(map[string][][model.MaxEventType]PerfMapStats),
		readLostEvents:    make(map[string][]uint64),
		sortingErrorStats: make(map[string][model.MaxEventType]*int64),
	}
	numCPU, err := utils.NumCPU()
	if err != nil {
		return nil, errors.Wrapf(err, "couldn't fetch the host CPU count")
	}
	pbm.numCPU = numCPU

	// compute statsMapPerfMap
	for perfMap, statsMap := range pbm.perfBufferMapNameToStatsMapsName {
		pbm.statsMapsNameToPerfBufferMapName[statsMap] = perfMap
	}

	// Select perf buffer statistics maps
	for perfMapName, statsMapName := range pbm.perfBufferMapNameToStatsMapsName {
		stats, ok, err := p.manager.GetMap(statsMapName)
		if !ok {
			return nil, errors.Errorf("map %s not found", statsMapName)
		}
		if err != nil {
			return nil, err
		}

		pbm.perfBufferStatsMaps[perfMapName] = stats
		// set default perf buffer size, it will be readjusted in the next loop if needed
		pbm.perfBufferSize[perfMapName] = float64(p.managerOptions.DefaultPerfRingBufferSize)
	}

	// Prepare user space counters
	for _, m := range p.manager.PerfMaps {
		var stats, kernelStats [][model.MaxEventType]PerfMapStats
		var usrLostEvents []uint64
		var sortingErrorStats [model.MaxEventType]*int64

		for i := 0; i < pbm.numCPU; i++ {
			stats = append(stats, [model.MaxEventType]PerfMapStats{})
			kernelStats = append(kernelStats, [model.MaxEventType]PerfMapStats{})
			usrLostEvents = append(usrLostEvents, 0)
		}

		for i := 0; i < int(model.MaxEventType); i++ {
			zero := int64(0)
			sortingErrorStats[i] = &zero
		}

		pbm.stats[m.Name] = stats
		pbm.kernelStats[m.Name] = kernelStats
		pbm.readLostEvents[m.Name] = usrLostEvents
		pbm.sortingErrorStats[m.Name] = sortingErrorStats

		// update perf buffer size if needed
		if m.PerfRingBufferSize != 0 {
			pbm.perfBufferSize[m.Name] = float64(m.PerfRingBufferSize)
		}
	}
	log.Debugf("monitoring perf ring buffer on %d CPU, %d events", pbm.numCPU, model.MaxEventType)
	return &pbm, nil
}

// getLostCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getLostCount(perfMap string, cpu int) uint64 {
	return atomic.LoadUint64(&pbm.readLostEvents[perfMap][cpu])
}

// GetLostCount returns the number of lost events for a given map and cpu. If a cpu of -1 is provided, the function will
// return the sum of all the lost events of all the cpus.
func (pbm *PerfBufferMonitor) GetLostCount(perfMap string, cpu int) uint64 {
	var total uint64

	switch {
	case cpu == -1:
		for i := range pbm.readLostEvents[perfMap] {
			total += pbm.getLostCount(perfMap, i)
		}
		break
	case cpu >= 0 && pbm.numCPU > cpu:
		total += pbm.getLostCount(perfMap, cpu)
	}

	return total
}

// getKernelLostCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getKernelLostCount(eventType model.EventType, perfMap string, cpu int) uint64 {
	return atomic.LoadUint64(&pbm.kernelStats[perfMap][cpu][eventType].Lost)
}

// GetAndResetKernelLostCount returns the number of lost events for a given map and cpu. If a cpu of -1 is provided, the function will
// return the sum of all the lost events of all the cpus.
func (pbm *PerfBufferMonitor) GetAndResetKernelLostCount(perfMap string, cpu int, evtTypes ...model.EventType) uint64 {
	var total uint64
	var shouldCount bool

	// query the kernel maps
	_ = pbm.collectAndSendKernelStats(nil)

	for cpuID := range pbm.kernelStats[perfMap] {
		if cpu == -1 || cpu == cpuID {
			for kernelEvtType := range pbm.kernelStats[perfMap][cpuID] {
				shouldCount = len(evtTypes) == 0
				if !shouldCount {
					for evtType := range evtTypes {
						if evtType == kernelEvtType {
							shouldCount = true
						}
					}
				}
				if shouldCount {
					total += pbm.getKernelLostCount(model.EventType(kernelEvtType), perfMap, cpuID)
				}
			}
		}
	}

	return total
}

// getAndResetReadLostCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getAndResetReadLostCount(perfMap string, cpu int) uint64 {
	return atomic.SwapUint64(&pbm.readLostEvents[perfMap][cpu], 0)
}

// GetAndResetLostCount returns the number of lost events and resets the counter for a given map and cpu. If a cpu of -1 is
// provided, the function will reset the counters of all the cpus for the provided map, and return the sum of all the
// lost events of all the cpus of the provided map.
func (pbm *PerfBufferMonitor) GetAndResetLostCount(perfMap string, cpu int) uint64 {
	var total uint64

	switch {
	case cpu == -1:
		for i := range pbm.readLostEvents[perfMap] {
			total += pbm.getAndResetReadLostCount(perfMap, i)
		}
		break
	case cpu >= 0 && pbm.numCPU > cpu:
		total += pbm.getAndResetReadLostCount(perfMap, cpu)
	}
	return total
}

// getEventCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getEventCount(eventType model.EventType, perfMap string, cpu int) uint64 {
	return atomic.LoadUint64(&pbm.stats[perfMap][cpu][eventType].Count)
}

// getEventBytes is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getEventBytes(eventType model.EventType, perfMap string, cpu int) uint64 {
	return atomic.LoadUint64(&pbm.stats[perfMap][cpu][eventType].Bytes)
}

// getKernelEventCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) swapKernelEventCount(eventType model.EventType, perfMap string, cpu int, value uint64) uint64 {
	return atomic.SwapUint64(&pbm.kernelStats[perfMap][cpu][eventType].Count, value)
}

// getKernelEventBytes is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) swapKernelEventBytes(eventType model.EventType, perfMap string, cpu int, value uint64) uint64 {
	return atomic.SwapUint64(&pbm.kernelStats[perfMap][cpu][eventType].Bytes, value)
}

// getKernelLostCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) swapKernelLostCount(eventType model.EventType, perfMap string, cpu int, value uint64) uint64 {
	return atomic.SwapUint64(&pbm.kernelStats[perfMap][cpu][eventType].Lost, value)
}

// GetEventStats returns the number of received events of the specified type and resets the counter
func (pbm *PerfBufferMonitor) GetEventStats(eventType model.EventType, perfMap string, cpu int) PerfMapStats {
	var stats PerfMapStats
	var maps []string

	if eventType >= model.MaxEventType {
		return stats
	}

	switch {
	case len(perfMap) == 0:
		for m := range pbm.stats {
			maps = append(maps, m)
		}
		break
	case pbm.stats[perfMap] != nil:
		maps = append(maps, perfMap)
	}

	for _, m := range maps {

		switch {
		case cpu == -1:
			for i := range pbm.stats[m] {
				stats.Count += pbm.getEventCount(eventType, perfMap, i)
				stats.Bytes += pbm.getEventBytes(eventType, perfMap, i)
			}
			break
		case cpu >= 0 && pbm.numCPU > cpu:
			stats.Count += pbm.getEventCount(eventType, perfMap, cpu)
			stats.Bytes += pbm.getEventBytes(eventType, perfMap, cpu)
		}

	}
	return stats
}

// getAndResetEventCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getAndResetEventCount(eventType model.EventType, perfMap string, cpu int) uint64 {
	return atomic.SwapUint64(&pbm.stats[perfMap][cpu][eventType].Count, 0)
}

// getAndResetEventBytes is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getAndResetEventBytes(eventType model.EventType, perfMap string, cpu int) uint64 {
	return atomic.SwapUint64(&pbm.stats[perfMap][cpu][eventType].Bytes, 0)
}

// getAndResetSortingErrorCount is an internal function, it can segfault if its parameters are incorrect.
func (pbm *PerfBufferMonitor) getAndResetSortingErrorCount(eventType model.EventType, perfMap string) int64 {
	return atomic.SwapInt64(pbm.sortingErrorStats[perfMap][eventType], 0)
}

// CountLostEvent adds `count` to the counter of lost events
func (pbm *PerfBufferMonitor) CountLostEvent(count uint64, m *manager.PerfMap, cpu int) {
	// sanity check
	if (pbm.readLostEvents[m.Name] == nil) || (len(pbm.readLostEvents[m.Name]) <= cpu) {
		return
	}
	atomic.AddUint64(&pbm.readLostEvents[m.Name][cpu], count)
}

// CountEvent adds `count` to the counter of received events of the specified type
func (pbm *PerfBufferMonitor) CountEvent(eventType model.EventType, timestamp uint64, count uint64, size uint64, m *manager.PerfMap, cpu int) {
	// check event order
	if timestamp < pbm.lastTimestamp && pbm.lastTimestamp != 0 {
		atomic.AddInt64(pbm.sortingErrorStats[m.Name][eventType], 1)
		atomic.SwapUint64(&pbm.shouldBumpGeneration, 1)
	} else {
		pbm.lastTimestamp = timestamp
	}

	// sanity check
	if (pbm.stats[m.Name] == nil) || (len(pbm.stats[m.Name]) <= cpu) || (len(pbm.stats[m.Name][cpu]) <= int(eventType)) {
		return
	}

	atomic.AddUint64(&pbm.stats[m.Name][cpu][eventType].Count, count)
	atomic.AddUint64(&pbm.stats[m.Name][cpu][eventType].Bytes, size)
}

func (pbm *PerfBufferMonitor) sendEventsAndBytesReadStats(client *statsd.Client) error {
	var count int64
	var err error
	tags := []string{pbm.probe.config.StatsTagsCardinality, "", ""}

	for m := range pbm.stats {
		tags[1] = fmt.Sprintf("map:%s", m)
		for cpu := range pbm.stats[m] {
			for eventType := range pbm.stats[m][cpu] {
				evtType := model.EventType(eventType)
				tags[2] = fmt.Sprintf("event_type:%s", evtType)

				if count = int64(pbm.getAndResetEventCount(evtType, m, cpu)); count > 0 {
					if err = client.Count(metrics.MetricPerfBufferEventsRead, count, tags, 1.0); err != nil {
						return err
					}
				}

				if count = int64(pbm.getAndResetEventBytes(evtType, m, cpu)); count > 0 {
					if err = client.Count(metrics.MetricPerfBufferBytesRead, count, tags, 1.0); err != nil {
						return err
					}
				}

				if count = pbm.getAndResetSortingErrorCount(evtType, m); count > 0 {
					if err = pbm.statsdClient.Count(metrics.MetricPerfBufferSortingError, count, tags, 1.0); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (pbm *PerfBufferMonitor) sendLostEventsReadStats(client *statsd.Client) error {
	tags := []string{pbm.probe.config.StatsTagsCardinality, ""}

	for m := range pbm.readLostEvents {
		var total float64
		tags[1] = fmt.Sprintf("map:%s", m)

		for cpu := range pbm.readLostEvents[m] {
			if count := float64(pbm.getAndResetReadLostCount(m, cpu)); count > 0 {
				if err := client.Count(metrics.MetricPerfBufferLostRead, int64(count), tags, 1.0); err != nil {
					return err
				}
				total += count
			}
		}

		if total > 0 {
			pbm.probe.DispatchCustomEvent(
				NewEventLostReadEvent(m, total),
			)
		}
	}
	return nil
}

func (pbm *PerfBufferMonitor) collectAndSendKernelStats(client *statsd.Client) error {
	var (
		id       uint32
		iterator *lib.MapIterator
		tmpCount uint64
	)
	cpuStats := make([]PerfMapStats, pbm.numCPU)
	tags := []string{pbm.probe.config.StatsTagsCardinality, "", ""}

	// loop through the statistics buffers of each perf map
	for perfMapName, statsMap := range pbm.perfBufferStatsMaps {
		// total and perEvent are used for alerting
		var total uint64
		perEvent := map[string]uint64{}
		tags[1] = fmt.Sprintf("map:%s", perfMapName)

		// loop through all the values of the active buffer
		iterator = statsMap.Iterate()
		for iterator.Next(&id, &cpuStats) {
			if id == 0 {
				// first event type is 1
				continue
			}

			// retrieve event type from key
			evtType := model.EventType(id % uint32(model.MaxEventType))
			tags[2] = fmt.Sprintf("event_type:%s", evtType)

			// loop over each cpu entry
			for cpu, stats := range cpuStats {
				// sanity checks:
				//   - check if the computed cpu id is below the current cpu count
				//   - check if we collect some data on the provided perf map
				//   - check if the computed event id is below the current max event id
				if (pbm.stats[perfMapName] == nil) || (len(pbm.stats[perfMapName]) <= cpu) || (len(pbm.stats[perfMapName][cpu]) <= int(evtType)) {
					return nil
				}

				// make sure perEvent is properly initialized
				if _, ok := perEvent[evtType.String()]; !ok {
					perEvent[evtType.String()] = 0
				}

				// Update stats to avoid sending twice the same data points
				if tmpCount = pbm.swapKernelEventBytes(evtType, perfMapName, cpu, stats.Bytes); tmpCount <= stats.Bytes {
					stats.Bytes -= tmpCount
				}
				if tmpCount = pbm.swapKernelEventCount(evtType, perfMapName, cpu, stats.Count); tmpCount <= stats.Count {
					stats.Count -= tmpCount
				}
				if tmpCount = pbm.swapKernelLostCount(evtType, perfMapName, cpu, stats.Lost); tmpCount <= stats.Lost {
					stats.Lost -= tmpCount
				}

				// purge dentry resolver generation if needed
				if evtType == model.FileRenameEventType || evtType == model.FileUnlinkEventType || evtType == model.FileRmdirEventType {
					atomic.SwapUint64(&pbm.shouldBumpGeneration, 1)
				}

				if client != nil {
					if err := pbm.sendKernelStats(client, stats, tags); err != nil {
						return err
					}
				}
				total += stats.Lost
				perEvent[evtType.String()] += stats.Lost
			}
		}
		if iterator.Err() != nil {
			return errors.Wrapf(iterator.Err(), "failed to dump the statistics buffer of map %s", perfMapName)
		}

		// send an alert if events were lost
		if total > 0 {
			pbm.probe.DispatchCustomEvent(
				NewEventLostWriteEvent(perfMapName, perEvent),
			)
		}
	}
	return nil
}

func (pbm *PerfBufferMonitor) sendKernelStats(client *statsd.Client, stats PerfMapStats, tags []string) error {
	if stats.Count > 0 {
		if err := client.Count(metrics.MetricPerfBufferEventsWrite, int64(stats.Count), tags, 1.0); err != nil {
			return err
		}
	}

	if stats.Bytes > 0 {
		if err := client.Count(metrics.MetricPerfBufferBytesWrite, int64(stats.Bytes), tags, 1.0); err != nil {
			return err
		}
	}

	if stats.Lost > 0 {
		if err := client.Count(metrics.MetricPerfBufferLostWrite, int64(stats.Lost), tags, 1.0); err != nil {
			return err
		}
	}

	return nil
}

// SendStats send event stats using the provided statsd client
func (pbm *PerfBufferMonitor) SendStats() error {
	if err := pbm.collectAndSendKernelStats(pbm.statsdClient); err != nil {
		return err
	}

	if atomic.SwapUint64(&pbm.shouldBumpGeneration, 0) == 1 {
		pbm.probe.resolvers.DentryResolver.BumpCacheGenerations()
	}

	if err := pbm.sendEventsAndBytesReadStats(pbm.statsdClient); err != nil {
		return err
	}

	return pbm.sendLostEventsReadStats(pbm.statsdClient)
}
