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
	"fmt"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/kubelet/cm/cpumanager/topology"
	"k8s.io/utils/cpuset"
)

func TestAllocator_AllocateAndRelease(t *testing.T) {
	// 8 CPUs: 0-3 NUMA0, 4-7 NUMA1. Dedicated 0,1 (NUMA0), shared 2-7 (2,3,4,5 on NUMA0; 6,7 on NUMA1)
	topo := &topology.CPUTopology{
		NumCPUs:    8,
		NumCores:   4,
		NumSockets: 2,
		CPUDetails: map[int]topology.CPUInfo{
			0: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			1: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			2: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			3: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			4: {NUMANodeID: 0, SocketID: 0, CoreID: 2},
			5: {NUMANodeID: 0, SocketID: 0, CoreID: 2},
			6: {NUMANodeID: 1, SocketID: 1, CoreID: 0},
			7: {NUMANodeID: 1, SocketID: 1, CoreID: 0},
		},
	}
	dedicated := cpuset.New(0, 1)
	shared := cpuset.New(2, 3, 4, 5, 6, 7)

	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "state")

	alloc := NewAllocator(dedicated, shared, topo, checkpointPath)

	// Allocate m=1, n=2 for pod1 (dedicated from 0,1; shared from 2,3 on same NUMA0)
	got1, err := alloc.Allocate(types.UID("pod1"), 1, 2)
	if err != nil {
		t.Fatalf("Allocate pod1: %v", err)
	}
	if got1.Size() != 3 {
		t.Errorf("pod1 expected 3 CPUs, got %v", got1.String())
	}
	if !got1.Contains(0) && !got1.Contains(1) {
		t.Errorf("pod1 dedicated should be one of 0,1")
	}
	if !got1.Contains(2) && !got1.Contains(3) {
		t.Errorf("pod1 shared should be from 2,3 (same NUMA)")
	}

	// Allocate m=1, n=1 for pod2 (remaining dedicated; remaining shared in NUMA0)
	got2, err := alloc.Allocate(types.UID("pod2"), 1, 1)
	if err != nil {
		t.Fatalf("Allocate pod2: %v", err)
	}
	if got2.Size() != 2 {
		t.Errorf("pod2 expected 2 CPUs, got %v", got2.String())
	}

	// Idempotent
	got1Again, _ := alloc.Allocate(types.UID("pod1"), 1, 2)
	if !got1.Equals(got1Again) {
		t.Errorf("pod1 re-allocate should return same: %v vs %v", got1.String(), got1Again.String())
	}

	// Release pod1
	alloc.Release(types.UID("pod1"))
	// Allocate pod3 - gets pod1's freed resources (1 dedicated + 2 shared from NUMA0)
	got3, err := alloc.Allocate(types.UID("pod3"), 1, 2)
	if err != nil {
		t.Fatalf("Allocate pod3: %v", err)
	}
	if got3.Size() != 3 {
		t.Errorf("pod3 expected 3 CPUs, got %v", got3.String())
	}
}

func TestAllocator_CheckpointPersistence(t *testing.T) {
	topo := &topology.CPUTopology{
		NumCPUs:    4,
		NumCores:   2,
		NumSockets: 1,
		CPUDetails: map[int]topology.CPUInfo{
			0: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			1: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			2: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			3: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
		},
	}
	dedicated := cpuset.New(0, 1)
	shared := cpuset.New(2, 3)

	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "state")

	alloc1 := NewAllocator(dedicated, shared, topo, checkpointPath)
	got, err := alloc1.Allocate(types.UID("pod-a"), 1, 1)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got.Size() != 2 {
		t.Errorf("expected 2 CPUs, got %v", got.String())
	}

	// Simulate restart: new allocator, load from checkpoint
	alloc2 := NewAllocator(dedicated, shared, topo, checkpointPath)
	gotAgain, err := alloc2.Allocate(types.UID("pod-a"), 1, 1)
	if err != nil {
		t.Fatalf("Allocate after restore: %v", err)
	}
	if !got.Equals(gotAgain) {
		t.Errorf("after restore, pod-a should get same cpuset: %v vs %v", got.String(), gotAgain.String())
	}
}

func TestAllocator_NoCheckpointPath(t *testing.T) {
	alloc := NewAllocator(cpuset.New(0, 1), cpuset.New(2, 3), nil, "")
	got, err := alloc.Allocate(types.UID("x"), 1, 1)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got.Size() != 2 {
		t.Errorf("expected 2 CPUs, got %v", got.String())
	}
	// With empty checkpoint path, no file is written
}

func TestAllocator_NUMAPreferenceForShared(t *testing.T) {
	// NUMA0: 0-3, NUMA1: 4-7. Dedicated pool = 0,1 (NUMA0), shared = 2-7 (NUMA0 has 2,3; NUMA1 has 4-7)
	topo := &topology.CPUTopology{
		NumCPUs:    8,
		NumCores:   4,
		NumSockets: 2,
		CPUDetails: map[int]topology.CPUInfo{
			0: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			1: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			2: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			3: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			4: {NUMANodeID: 1, SocketID: 1, CoreID: 0},
			5: {NUMANodeID: 1, SocketID: 1, CoreID: 0},
			6: {NUMANodeID: 1, SocketID: 1, CoreID: 1},
			7: {NUMANodeID: 1, SocketID: 1, CoreID: 1},
		},
	}
	dedicated := cpuset.New(0, 1)
	shared := cpuset.New(2, 3, 4, 5, 6, 7)

	alloc := NewAllocator(dedicated, shared, topo, "")
	// Allocate m=1, n=2. Dedicated will get 0 or 1 (NUMA0). Shared should prefer NUMA0 (2,3).
	got, err := alloc.Allocate(types.UID("p"), 1, 2)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got.Size() != 3 {
		t.Errorf("expected 3 CPUs, got %v", got.String())
	}
	// Shared must be from 2,3 (same NUMA as dedicated 0,1)
	if !got.Contains(2) && !got.Contains(3) {
		t.Errorf("shared must be from same NUMA: got %v", got.String())
	}
}

func TestAllocatorWithSharedPools_RoundRobin(t *testing.T) {
	// 120 cores, strategy 20:4+8 -> 80 dedicated, 40 shared, 5 pools of 8 cores each
	topo := &topology.CPUTopology{
		NumCPUs:    120,
		NumCores:   60,
		NumSockets: 2,
		CPUDetails: make(map[int]topology.CPUInfo),
	}
	for i := 0; i < 120; i++ {
		topo.CPUDetails[i] = topology.CPUInfo{
			NUMANodeID: i / 60,
			SocketID:   i / 60,
			CoreID:     i / 2,
		}
	}
	dedicated := cpuset.New()
	for i := 0; i < 80; i++ {
		dedicated = dedicated.Union(cpuset.New(i))
	}
	// 5 shared pools of 8 cores each (80-87, 88-95, 96-103, 104-111, 112-119)
	sharedPools := []cpuset.CPUSet{
		cpuset.New(80, 81, 82, 83, 84, 85, 86, 87),
		cpuset.New(88, 89, 90, 91, 92, 93, 94, 95),
		cpuset.New(96, 97, 98, 99, 100, 101, 102, 103),
		cpuset.New(104, 105, 106, 107, 108, 109, 110, 111),
		cpuset.New(112, 113, 114, 115, 116, 117, 118, 119),
	}
	alloc := NewAllocatorWithSharedPools(dedicated, sharedPools, topo, "")

	// Allocate 20 instances: 4 dedicated + 8 shared each, round-robin across 5 pools
	for i := 0; i < 20; i++ {
		uid := types.UID(fmt.Sprintf("pod-%d", i))
		got, err := alloc.Allocate(uid, 4, 8)
		if err != nil {
			t.Fatalf("Allocate pod %d: %v", i, err)
		}
		if got.Size() != 12 {
			t.Errorf("pod %d expected 12 CPUs, got %v", i, got.String())
		}
		// Instance i should get shared from pool (i % 5) - verify intersection equals that pool
		expectedPool := sharedPools[i%5]
		gotShared := got.Intersection(cpuset.New(80, 81, 82, 83, 84, 85, 86, 87, 88, 89, 90, 91, 92, 93, 94, 95, 96, 97, 98, 99, 100, 101, 102, 103, 104, 105, 106, 107, 108, 109, 110, 111, 112, 113, 114, 115, 116, 117, 118, 119))
		if !gotShared.Equals(expectedPool) {
			t.Errorf("pod %d expected shared pool %v, got %v", i, expectedPool.String(), gotShared.String())
		}
	}
}

func TestAllocator_SharedOnlyFromSameNUMA_FailWhenInsufficient(t *testing.T) {
	// Dedicated 0,1 (NUMA0). Shared 2,3 (NUMA0, only 2 cores). Need n=3 -> should fail.
	topo := &topology.CPUTopology{
		NumCPUs:    4,
		NumCores:   2,
		NumSockets: 1,
		CPUDetails: map[int]topology.CPUInfo{
			0: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			1: {NUMANodeID: 0, SocketID: 0, CoreID: 0},
			2: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
			3: {NUMANodeID: 0, SocketID: 0, CoreID: 1},
		},
	}
	dedicated := cpuset.New(0, 1)
	shared := cpuset.New(2, 3)

	alloc := NewAllocator(dedicated, shared, topo, "")
	_, err := alloc.Allocate(types.UID("p"), 1, 3)
	if err == nil {
		t.Error("expected error when requesting 3 shared but only 2 in same NUMA")
	}
}
