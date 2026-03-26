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

package cpusetstate

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/koordinator-sh/koordinator/pkg/koordlet/resourceexecutor"
	"github.com/koordinator-sh/koordinator/pkg/util/cpuset"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/statesinformer"
	koordletutil "github.com/koordinator-sh/koordinator/pkg/koordlet/util"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/util/kubelet"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/util/system"
)

const (
	// CPUSetStatePath is the HTTP path for cpuset state API.
	CPUSetStatePath = "/cpuset-state"
)

var (
	globalStatesInformer statesinformer.StatesInformer
	globalMu             sync.RWMutex
)

// SetStatesInformer sets the states informer for the cpuset-state handler.
// Called by the agent when the daemon is created.
func SetStatesInformer(si statesinformer.StatesInformer) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalStatesInformer = si
}

// Handler returns the HTTP handler for /cpuset-state.
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		globalMu.RLock()
		si := globalStatesInformer
		globalMu.RUnlock()

		if si == nil {
			http.Error(w, "cpuset-state: states informer not ready", http.StatusServiceUnavailable)
			return
		}

		resp := buildCPUSetState(si)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			klog.Errorf("cpuset-state: encode response: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
	}
}

// CPUSetStateResponse is the JSON response for /cpuset-state.
type CPUSetStateResponse struct {
	Pods []PodCPUSetState `json:"pods"`
}

// PodCPUSetState is the cpuset state for a single pod.
type PodCPUSetState struct {
	PodUID       string `json:"podUID"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Intended     string `json:"intended"`     // from m+n checkpoint
	Actual       string `json:"actual"`       // from cgroup cpuset.cpus
	WriteStatus  string `json:"writeStatus"`  // "ok", "mismatch", or "unknown" (when intended is empty)
	CgroupPath   string `json:"cgroupPath"`   // cgroup path for cpuset
	LastError   string `json:"lastError,omitempty"` // empty for now; future: from failure tracking
}

func buildCPUSetState(si statesinformer.StatesInformer) CPUSetStateResponse {
	resp := CPUSetStateResponse{Pods: []PodCPUSetState{}}

	checkpoint := readCheckpoint()
	reader := resourceexecutor.NewCgroupReader()

	podMetas := si.GetAllPods()
	for _, meta := range podMetas {
		pod := meta.Pod
		if pod == nil {
			continue
		}

		intended := ""
		if checkpoint != nil {
			if e, ok := checkpoint.Entries[string(pod.UID)]; ok {
				intended = mergeCPUSetStrings(e.Dedicated, e.Shared)
			}
		}

		podCgroupDir := koordletutil.GetPodCgroupParentDir(pod)
		actual, actualSet, cgroupPath := readActualCPUSet(reader, pod, podCgroupDir)

		writeStatus := "unknown" // when intended is empty, we cannot determine if write succeeded
		if intended != "" {
			writeStatus = "ok"
			if actual == "" {
				writeStatus = "mismatch"
			} else if actualSet != nil {
				intendedSet, parseErr := cpuset.Parse(intended)
				if parseErr == nil && !intendedSet.Equals(*actualSet) {
					writeStatus = "mismatch"
				}
			}
		}

		resp.Pods = append(resp.Pods, PodCPUSetState{
			PodUID:      string(pod.UID),
			Namespace:   pod.Namespace,
			Name:        pod.Name,
			Intended:    intended,
			Actual:      actual,
			WriteStatus: writeStatus,
			CgroupPath:  cgroupPath,
		})
	}

	return resp
}

// readActualCPUSet reads cpuset from container-level cgroup when available (cpuset is applied at container level).
// Falls back to pod-level when no container cgroup can be resolved. Returns (actual string, actualSet, cgroupPath).
func readActualCPUSet(reader resourceexecutor.CgroupReader, pod *corev1.Pod, podCgroupDir string) (string, *cpuset.CPUSet, string) {
	// Try container-level first (where cpuset rule actually writes)
	containerStatuses := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	containerStatuses = append(containerStatuses, pod.Status.ContainerStatuses...)
	for i := range containerStatuses {
		cs := &containerStatuses[i]
		if cs.ContainerID == "" {
			continue
		}
		containerDir, err := koordletutil.GetContainerCgroupParentDir(podCgroupDir, cs)
		if err != nil {
			continue
		}
		actualSet, err := reader.ReadCPUSet(containerDir)
		if err == nil && actualSet != nil {
			if r, err := system.GetCgroupResource(system.CPUSetCPUSName); err == nil {
				return actualSet.String(), actualSet, r.Path(containerDir)
			}
			return actualSet.String(), actualSet, ""
		}
	}
	// Fallback to pod-level
	actualSet, err := reader.ReadCPUSet(podCgroupDir)
	if err == nil && actualSet != nil {
		cgroupPath := ""
		if r, err := system.GetCgroupResource(system.CPUSetCPUSName); err == nil {
			cgroupPath = r.Path(podCgroupDir)
		}
		return actualSet.String(), actualSet, cgroupPath
	}
	return "", nil, ""
}

type checkpointData struct {
	Entries map[string]struct {
		Dedicated string `json:"dedicated"`
		Shared    string `json:"shared"`
	} `json:"entries"`
}

// readCheckpoint reads m+n checkpoint. The allocator writes to either:
// - cpuset_m_plus_n_state (whole-node allocator)
// - cpuset_m_plus_n_state_numa_0, _numa_1, ... (per-NUMA allocator)
// We read all matching files and merge entries.
func readCheckpoint() *checkpointData {
	basePath := kubelet.GetCPUSetMPlusNStateFilePath(system.Conf.GetCpusetCheckpointRoot())
	dir := filepath.Dir(basePath)
	baseName := filepath.Base(basePath)

	// Collect all checkpoint paths: base + per-NUMA
	var paths []string
	// Try base file first
	if _, err := os.Stat(basePath); err == nil {
		paths = append(paths, basePath)
	}
	// Glob per-NUMA files: cpuset_m_plus_n_state_numa_*
	matches, err := filepath.Glob(filepath.Join(dir, baseName+"_numa_*"))
	if err == nil {
		paths = append(paths, matches...)
	}

	merged := &checkpointData{Entries: make(map[string]struct {
		Dedicated string `json:"dedicated"`
		Shared    string `json:"shared"`
	})}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			klog.V(5).Infof("cpuset-state: read checkpoint %s: %v", p, err)
			continue
		}
		var raw checkpointData
		if json.Unmarshal(data, &raw) != nil || raw.Entries == nil {
			continue
		}
		for uid, e := range raw.Entries {
			if _, exists := merged.Entries[uid]; !exists {
				merged.Entries[uid] = e
			}
		}
	}
	if len(merged.Entries) == 0 && len(paths) == 0 {
		klog.V(5).Infof("cpuset-state: no checkpoint found (tried %s and %s_numa_*)", basePath, basePath)
		return nil
	}
	return merged
}

func mergeCPUSetStrings(a, b string) string {
	if a == "" && b == "" {
		return ""
	}
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	setA, errA := cpuset.Parse(a)
	setB, errB := cpuset.Parse(b)
	if errA != nil || errB != nil {
		if a != "" && b != "" {
			return a + "," + b
		}
		return a + b
	}
	return setA.Union(setB).String()
}
