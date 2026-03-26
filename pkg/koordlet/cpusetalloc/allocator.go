/*
Copyright 2022 The Koordinator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cpusetalloc

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/utils/cpuset"

	"github.com/koordinator-sh/koordinator/pkg/koordlet/util/kubelet"
)

const checkpointTmpSuffix = ".tmp"

// checkpointData is the persisted format for m+n allocator state.
type checkpointData struct {
	Entries map[string]entry `json:"entries"`
}

type entry struct {
	Dedicated string `json:"dedicated"`
	Shared    string `json:"shared"`
}

// Allocator manages m+n CPU allocation: m dedicated + n shared cores per pod.
// Supports two modes:
// - Single shared pool: allocate n cores from sharedPool with NUMA affinity.
// - Multi shared pools: round-robin assign each pod to a pool, multiple pods share the same pool for load balancing.
type Allocator struct {
	mu sync.RWMutex

	dedicatedPool  cpuset.CPUSet
	sharedPool     cpuset.CPUSet
	sharedPools    []cpuset.CPUSet // when set, use round-robin assignment instead of allocating from sharedPool
	topology       *topology.CPUTopology
	checkpointPath string

	// allocatedDedicated: podUID -> cpuset (m cores)
	allocatedDedicated map[types.UID]cpuset.CPUSet
	// allocatedShared: podUID -> cpuset (n cores, or whole pool when using sharedPools)
	allocatedShared map[types.UID]cpuset.CPUSet
}

// NewAllocator creates an m+n allocator with a single shared pool.
// dedicatedPool: CPUs for exclusive allocation (m).
// sharedPool: CPUs for shared allocation (n), typically from sharePools.
// checkpointPath: host path for persisting state (e.g. /var/lib/kubelet/cpuset_m_plus_n_state). Empty disables persistence.
func NewAllocator(dedicatedPool, sharedPool cpuset.CPUSet, topo *topology.CPUTopology, checkpointPath string) *Allocator {
	return newAllocator(dedicatedPool, sharedPool, nil, topo, checkpointPath)
}

// NewAllocatorWithSharedPools creates an m+n allocator with multiple shared pools for round-robin load balancing.
// Each pool has n cores. Instances are assigned to pools in round-robin: instance i gets sharedPools[i % len(sharedPools)].
// Multiple instances share the same pool (e.g. 20 instances, 5 pools -> 4 instances per pool).
func NewAllocatorWithSharedPools(dedicatedPool cpuset.CPUSet, sharedPools []cpuset.CPUSet, topo *topology.CPUTopology, checkpointPath string) *Allocator {
	return newAllocator(dedicatedPool, cpuset.New(), sharedPools, topo, checkpointPath)
}

func newAllocator(dedicatedPool, sharedPool cpuset.CPUSet, sharedPools []cpuset.CPUSet, topo *topology.CPUTopology, checkpointPath string) *Allocator {
	a := &Allocator{
		dedicatedPool:      dedicatedPool,
		sharedPool:         sharedPool,
		sharedPools:        sharedPools,
		topology:           topo,
		checkpointPath:     checkpointPath,
		allocatedDedicated: make(map[types.UID]cpuset.CPUSet),
		allocatedShared:    make(map[types.UID]cpuset.CPUSet),
	}
	if checkpointPath != "" {
		if err := a.loadCheckpoint(); err != nil {
			klog.V(4).Infof("m+n allocator: load checkpoint failed (ignored): %v", err)
		}
		// Ensure checkpoint file exists (e.g. after manual deletion). Create empty file if missing.
		if _, err := os.Stat(checkpointPath); os.IsNotExist(err) {
			a.mu.Lock()
			_ = a.saveCheckpointLocked()
			a.mu.Unlock()
		}
	}
	return a
}

// Allocate allocates m dedicated + n shared CPUs for the pod.
// Shared cores are allocated with NUMA affinity to the dedicated cores (same NUMA preferred).
// Returns the union cpuset (m + n) or error.
func (a *Allocator) Allocate(podUID types.UID, m, n int) (cpuset.CPUSet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if existing, ok := a.allocatedDedicated[podUID]; ok {
		// already allocated, return existing
		shared := a.allocatedShared[podUID]
		return existing.Union(shared), nil
	}

	availableDedicated := a.dedicatedPool
	for _, c := range a.allocatedDedicated {
		availableDedicated = availableDedicated.Difference(c)
	}

	var dedicated cpuset.CPUSet
	if m > 0 {
		var err error
		if a.topology != nil {
			dedicated, err = kubelet.TakeByTopology(availableDedicated, m, a.topology)
		} else {
			dedicated, err = takeFirstN(availableDedicated, m)
		}
		if err != nil {
			return cpuset.New(), fmt.Errorf("allocate %d dedicated cores: %w", m, err)
		}
	}

	var shared cpuset.CPUSet
	if n > 0 {
		if len(a.sharedPools) > 0 {
			// Round-robin: assign whole pool to instance (multiple instances share same pool for load balancing)
			poolIndex := len(a.allocatedDedicated) % len(a.sharedPools)
			shared = a.sharedPools[poolIndex]
			if shared.Size() < n {
				return cpuset.New(), fmt.Errorf("shared pool %d has %d cores, need %d", poolIndex, shared.Size(), n)
			}
		} else {
			availableShared := a.sharedPool
			for _, c := range a.allocatedShared {
				availableShared = availableShared.Difference(c)
			}
			var err error
			shared, err = takeSharedByNUMAPreference(availableShared, dedicated, n, a.topology)
			if err != nil {
				return cpuset.New(), fmt.Errorf("allocate %d shared cores: %w", n, err)
			}
		}
	}

	a.allocatedDedicated[podUID] = dedicated
	a.allocatedShared[podUID] = shared

	if a.checkpointPath != "" {
		if err := a.saveCheckpointLocked(); err != nil {
			klog.V(4).Infof("m+n allocator: save checkpoint failed: %v", err)
		}
	}

	result := dedicated.Union(shared)
	klog.V(5).Infof("m+n allocator: allocated pod %v -> dedicated %v, shared %v, total %v",
		podUID, dedicated.String(), shared.String(), result.String())
	return result, nil
}

// GetAllocation returns the allocated cpuset for the pod if it exists.
func (a *Allocator) GetAllocation(podUID types.UID) (cpuset.CPUSet, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	dedicated, ok := a.allocatedDedicated[podUID]
	if !ok {
		return cpuset.New(), false
	}
	shared := a.allocatedShared[podUID]
	return dedicated.Union(shared), true
}

// Release releases the allocation for the pod.
func (a *Allocator) Release(podUID types.UID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocatedDedicated, podUID)
	delete(a.allocatedShared, podUID)
	if a.checkpointPath != "" {
		_ = a.saveCheckpointLocked()
	}
	klog.V(5).Infof("m+n allocator: released pod %v", podUID)
}

// ReleaseStale releases allocations for pods not in keepUIDs.
func (a *Allocator) ReleaseStale(keepUIDs map[types.UID]struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := false
	for uid := range a.allocatedDedicated {
		if _, keep := keepUIDs[uid]; !keep {
			delete(a.allocatedDedicated, uid)
			delete(a.allocatedShared, uid)
			changed = true
			klog.V(5).Infof("m+n allocator: released stale pod %v", uid)
		}
	}
	if changed && a.checkpointPath != "" {
		_ = a.saveCheckpointLocked()
	}
}

// takeSharedByNUMAPreference allocates n CPUs from availableShared, preferring the same NUMA node(s) as dedicated.
// Uses TakeByTopology for NUMA-aware allocation (same algorithm as kubelet).
func takeSharedByNUMAPreference(availableShared, dedicated cpuset.CPUSet, n int, topo *topology.CPUTopology) (cpuset.CPUSet, error) {
	if availableShared.Size() < n {
		return cpuset.New(), fmt.Errorf("not enough shared CPUs: need %d, have %d", n, availableShared.Size())
	}
	if topo == nil {
		return takeFirstN(availableShared, n)
	}
	// Only allocate shared from the same NUMA node(s) as dedicated.
	if dedicated.Size() > 0 {
		numaIDs := getNUMANodesFromCPUs(dedicated, topo)
		if len(numaIDs) == 0 {
			return cpuset.New(), fmt.Errorf("cannot determine NUMA for dedicated cpuset %v", dedicated.String())
		}
		sharedInSameNUMAs := topo.CPUDetails.CPUsInNUMANodes(numaIDs...).Intersection(availableShared)
		if sharedInSameNUMAs.Size() < n {
			return cpuset.New(), fmt.Errorf("not enough shared CPUs in same NUMA(s) as dedicated: need %d, have %d in NUMA %v",
				n, sharedInSameNUMAs.Size(), numaIDs)
		}
		return kubelet.TakeByTopology(sharedInSameNUMAs, n, topo)
	}
	return kubelet.TakeByTopology(availableShared, n, topo)
}

func getNUMANodesFromCPUs(cpus cpuset.CPUSet, topo *topology.CPUTopology) []int {
	seen := make(map[int]struct{})
	for _, cpuID := range cpus.UnsortedList() {
		if info, ok := topo.CPUDetails[cpuID]; ok {
			seen[info.NUMANodeID] = struct{}{}
		}
	}
	ids := make([]int, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// takeFirstN takes the first n CPUs from the set (sorted by CPU ID).
func takeFirstN(c cpuset.CPUSet, n int) (cpuset.CPUSet, error) {
	if c.Size() < n {
		return cpuset.New(), fmt.Errorf("not enough CPUs: need %d, have %d", n, c.Size())
	}
	list := c.UnsortedList()
	sort.Ints(list)
	result := cpuset.New()
	for i := 0; i < n && i < len(list); i++ {
		result = result.Union(cpuset.New(list[i]))
	}
	return result, nil
}

func (a *Allocator) saveCheckpointLocked() error {
	data := checkpointData{
		Entries: make(map[string]entry),
	}
	for uid, d := range a.allocatedDedicated {
		s := a.allocatedShared[uid]
		data.Entries[string(uid)] = entry{
			Dedicated: d.String(),
			Shared:    s.String(),
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.checkpointPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmpPath := a.checkpointPath + checkpointTmpSuffix
	if err := os.WriteFile(tmpPath, raw, 0644); err != nil {
		return err
	}
	return os.Rename(tmpPath, a.checkpointPath)
}

func (a *Allocator) loadCheckpoint() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	raw, err := os.ReadFile(a.checkpointPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var data checkpointData
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	for uidStr, e := range data.Entries {
		dedicated, err := cpuset.Parse(e.Dedicated)
		if err != nil {
			klog.V(4).Infof("m+n allocator: skip invalid dedicated cpuset %q for %s: %v", e.Dedicated, uidStr, err)
			continue
		}
		shared, err := cpuset.Parse(e.Shared)
		if err != nil {
			klog.V(4).Infof("m+n allocator: skip invalid shared cpuset %q for %s: %v", e.Shared, uidStr, err)
			continue
		}
		// Validate: dedicated must be in pool, shared must be in pool(s)
		if !dedicated.IsSubsetOf(a.dedicatedPool) && dedicated.Size() > 0 {
			klog.V(4).Infof("m+n allocator: skip restored %s: dedicated %v not in pool", uidStr, dedicated)
			continue
		}
		sharedValid := false
		if len(a.sharedPools) > 0 {
			for _, p := range a.sharedPools {
				if shared.IsSubsetOf(p) && shared.Size() > 0 {
					sharedValid = true
					break
				}
			}
			if shared.Size() == 0 {
				sharedValid = true
			}
		} else {
			sharedValid = shared.IsSubsetOf(a.sharedPool) || shared.Size() == 0
		}
		if !sharedValid {
			klog.V(4).Infof("m+n allocator: skip restored %s: shared %v not in pool(s)", uidStr, shared)
			continue
		}
		a.allocatedDedicated[types.UID(uidStr)] = dedicated
		a.allocatedShared[types.UID(uidStr)] = shared
	}
	klog.V(4).Infof("m+n allocator: restored %d entries from checkpoint", len(a.allocatedDedicated))
	return nil
}
