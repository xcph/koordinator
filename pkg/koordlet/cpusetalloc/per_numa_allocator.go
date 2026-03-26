/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cpusetalloc

import (
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/utils/cpuset"

	"github.com/koordinator-sh/koordinator/apis/extension"
)

// PerNUMAAllocator allocates m+n CPUs per NUMA node. Each NUMA has its own dedicated/shared pools
// and strategy (instanceCap, m, n). Allocation is round-robin across NUMA nodes.
type PerNUMAAllocator struct {
	mu sync.RWMutex

	numaAllocators map[int32]*Allocator // key: NUMA node ID
	numaOrder       []int32              // sorted NUMA IDs for deterministic round-robin
	numaStrategies  map[int32]*extension.CPUExclusiveSharedStrategy
	nextNUMAIndex   atomic.Uint32
}

// NewPerNUMAAllocator creates a per-NUMA allocator. numaPools maps NUMA ID to (dedicatedSet, sharedSet or sharedPools).
// numaStrategies maps NUMA ID to strategy for LSR/LSE pods (m,n when caller passes 0,0).
func NewPerNUMAAllocator(
	numaPools map[int32]struct {
		DedicatedSet cpuset.CPUSet
		SharedSet    cpuset.CPUSet
		SharedPools  []cpuset.CPUSet
	},
	numaStrategies map[int32]*extension.CPUExclusiveSharedStrategy,
	topo *topology.CPUTopology,
	checkpointPath string,
) *PerNUMAAllocator {
	allocators := make(map[int32]*Allocator)
	order := make([]int32, 0, len(numaPools))
	for numaID, pools := range numaPools {
		cp := ""
		if checkpointPath != "" {
			cp = checkpointPath + "_numa_" + strconv.Itoa(int(numaID))
		}
		var alloc *Allocator
		if len(pools.SharedPools) > 0 {
			alloc = NewAllocatorWithSharedPools(pools.DedicatedSet, pools.SharedPools, topo, cp)
		} else {
			alloc = NewAllocator(pools.DedicatedSet, pools.SharedSet, topo, cp)
		}
		allocators[numaID] = alloc
		order = append(order, numaID)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	return &PerNUMAAllocator{
		numaAllocators: allocators,
		numaOrder:      order,
		numaStrategies: numaStrategies,
	}
}

// Allocate allocates m+n CPUs for the pod. When m=0 and n=0 (LSR/LSE without annotation),
// uses the NUMA's strategy (DedicatedCores, SharedCores). Tries each NUMA in round-robin until one succeeds.
func (p *PerNUMAAllocator) Allocate(podUID types.UID, m, n int) (cpuset.CPUSet, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.numaOrder) == 0 {
		return cpuset.New(), fmt.Errorf("per-NUMA allocator has no NUMA pools")
	}

	// Check if already allocated (any NUMA)
	for _, alloc := range p.numaAllocators {
		if cset, ok := alloc.GetAllocation(podUID); ok {
			return cset, nil
		}
	}

	start := p.nextNUMAIndex.Add(1) - 1
	for i := 0; i < len(p.numaOrder); i++ {
		idx := (int(start) + i) % len(p.numaOrder)
		numaID := p.numaOrder[idx]
		alloc := p.numaAllocators[numaID]
		strat := p.numaStrategies[numaID]

		useM, useN := m, n
		if useM == 0 && useN == 0 && strat != nil {
			useM = int(strat.DedicatedCores)
			useN = int(strat.SharedCores)
		}
		if useM <= 0 {
			continue
		}

		result, err := alloc.Allocate(podUID, useM, useN)
		if err == nil && result.Size() > 0 {
			p.nextNUMAIndex.Store(uint32((idx + 1) % len(p.numaOrder)))
			klog.V(5).Infof("per-NUMA allocator: allocated pod %v on NUMA %d -> %v", podUID, numaID, result.String())
			return result, nil
		}
		// This NUMA failed (no capacity), try next
	}

	return cpuset.New(), fmt.Errorf("no capacity on any NUMA for m=%d n=%d", m, n)
}

// ReleaseStale releases allocations for pods not in keepUIDs across all NUMA allocators.
func (p *PerNUMAAllocator) ReleaseStale(keepUIDs map[types.UID]struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, alloc := range p.numaAllocators {
		alloc.ReleaseStale(keepUIDs)
	}
}
