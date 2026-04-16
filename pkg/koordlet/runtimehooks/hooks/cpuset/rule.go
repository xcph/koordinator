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

package cpuset

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	topov1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/cpuset"
	"k8s.io/utils/ptr"

	"github.com/koordinator-sh/koordinator/apis/extension"
	"github.com/koordinator-sh/koordinator/pkg/features"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/cpusetalloc"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/metrics"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/runtimehooks/protocol"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/statesinformer"
	koordletutil "github.com/koordinator-sh/koordinator/pkg/koordlet/util"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/util/kubelet"
	"github.com/koordinator-sh/koordinator/pkg/koordlet/util/system"
	"github.com/koordinator-sh/koordinator/pkg/util"
)

// mPlusNAllocatorInterface is implemented by both Allocator and PerNUMAAllocator.
type mPlusNAllocatorInterface interface {
	Allocate(podUID types.UID, m, n int) (cpuset.CPUSet, error)
	ReleaseStale(keepUIDs map[types.UID]struct{})
}

type cpusetRule struct {
	kubeletPolicy   extension.KubeletCPUManagerPolicy
	sharePools      []extension.CPUSharedPool
	beSharePools    []extension.CPUSharedPool
	systemQOSCPUSet string
	// m+n allocation
	mPlusNAllocator mPlusNAllocatorInterface
	strategy        *extension.CPUExclusiveSharedStrategy
	parsedStrategy  *extension.ParsedCPUExclusiveSharedStrategy
	// releaseStaleBeforeAllocate clears checkpoint entries for removed pods before allocating.
	// Needed because PreCreateContainer/runtime hook can run before ruleUpdateCb, leaving stale entries.
	releaseStaleBeforeAllocate func()
	// TODO: support per-node disable
}

func (r *cpusetRule) getContainerCPUSet(containerReq *protocol.ContainerRequest) (*string, error) {
	// pod specifies QoS=BE and share pool id in annotations, use part be cpu share pool if BECPUManager enabled
	// pod specifies share pool id in annotations, use part cpu share pool
	// pod specifies QoS=SYSTEM in labels, use system qos resource if rule exist
	// pod specifies QoS=LS in labels, use all share pool
	// besteffort pod(including QoS=BE) will be managed by cpu suppress policy, inject empty string
	// guaranteed/bustable pod without QoS label, if kubelet use none policy, use all share pool, and if kubelet use
	// static policy, do nothing
	if containerReq == nil {
		return nil, nil
	}
	podAnnotations := containerReq.PodAnnotations
	podLabels := containerReq.PodLabels

	// m+n allocation: Koordinator forces m+n cpuset onto pods according to node cpuExclusiveSharedStrategy.
	// 1) Pod annotation cpu-exclusive-cores: m from pod, n from pod or strategy.
	// 2) Pod with LSR/LSE QoS and no annotation: m and n from node strategy (forced by Koordinator).
	// For per-NUMA strategy, pass m=0,n=0 so allocator uses the NUMA's own m,n.
	hasStrategy := r.strategy != nil || (r.parsedStrategy != nil && r.parsedStrategy.PerNUMA != nil)
	if r.mPlusNAllocator != nil && hasStrategy {
		var m, n int32
		usePerNUMAStrategy := r.parsedStrategy != nil && r.parsedStrategy.PerNUMA != nil
		if podM, ok := extension.GetPodCPUExclusiveCores(podAnnotations); ok {
			m = podM
			if podN, hasN := extension.GetPodCPUSharedCores(podAnnotations); hasN {
				n = podN
			} else if r.strategy != nil {
				n = r.strategy.SharedCores
			} else if usePerNUMAStrategy {
				for _, s := range r.parsedStrategy.PerNUMA {
					n = s.SharedCores
					break
				}
			}
		} else {
			podQOS := extension.GetQoSClassByAttrs(podLabels, podAnnotations)
			if podQOS == extension.QoSLSR || podQOS == extension.QoSLSE {
				if usePerNUMAStrategy {
					m, n = 0, 0 // allocator uses per-NUMA strategy
				} else if r.strategy != nil {
					m = r.strategy.DedicatedCores
					n = r.strategy.SharedCores
				}
			}
		}
		shouldAllocate := m > 0 || (usePerNUMAStrategy && m == 0 && n == 0)
		if shouldAllocate {
			if r.releaseStaleBeforeAllocate != nil {
				r.releaseStaleBeforeAllocate()
			}
			if cpusetVal, err := r.mPlusNAllocator.Allocate(types.UID(containerReq.PodMeta.UID), int(m), int(n)); err != nil {
				klog.V(4).Infof("m+n allocator failed for container %v/%v: %v",
					containerReq.PodMeta.String(), containerReq.ContainerMeta.Name, err)
				return nil, err
			} else if cpusetVal.Size() > 0 {
				s := cpusetVal.String()
				klog.V(6).Infof("get cpuset from m+n allocator for container %v/%v: %v",
					containerReq.PodMeta.String(), containerReq.ContainerMeta.Name, s)
				return ptr.To[string](s), nil
			}
		}
	}

	podAlloc, err := extension.GetResourceStatus(podAnnotations)
	if err != nil {
		return nil, err
	}

	podQOSClass := extension.GetQoSClassByAttrs(podLabels, podAnnotations)

	// check if numa-aware
	isNUMAAware := false
	for _, numaNode := range podAlloc.NUMANodeResources {
		if numaNode.Resources == nil {
			continue
		}
		// check if cpu resource is allocated in numa-level since there can be numa allocation without cpu
		if !numaNode.Resources.Cpu().IsZero() ||
			util.GetBatchMilliCPUFromResourceList(numaNode.Resources) > 0 {
			isNUMAAware = true
			break
		}
	}
	if isNUMAAware {
		getCPUFromSharePoolByAllocFn := func(sharePools []extension.CPUSharedPool, alloc *extension.ResourceStatus) string {
			cpusetList := make([]string, 0, len(alloc.NUMANodeResources))
			for _, numaNode := range alloc.NUMANodeResources {
				for _, nodeSharePool := range sharePools {
					if numaNode.Node == nodeSharePool.Node {
						cpusetList = append(cpusetList, nodeSharePool.CPUSet)
					}
				}
			}
			return strings.Join(cpusetList, ",")
		}
		if podQOSClass == extension.QoSBE && features.DefaultKoordletFeatureGate.Enabled(features.BECPUManager) {
			// BE pods which have specified cpu share pool
			cpuSetStr := getCPUFromSharePoolByAllocFn(r.beSharePools, podAlloc)
			klog.V(6).Infof("get cpuset from specified be cpushare pool for container %v/%v",
				containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
			return ptr.To[string](cpuSetStr), nil
		} else if podQOSClass != extension.QoSBE {
			// LS pods which have specified cpu share pool
			cpuSetStr := getCPUFromSharePoolByAllocFn(r.sharePools, podAlloc)
			klog.V(6).Infof("get cpuset from specified cpushare pool for container %v/%v",
				containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
			return ptr.To[string](cpuSetStr), nil
		}
	}

	// SYSTEM QoS cpuset
	// TBD: support numa-aware
	if podQOSClass == extension.QoSSystem && len(r.systemQOSCPUSet) > 0 {
		klog.V(6).Infof("get cpuset from system qos rule for container %s/%s",
			containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
		return ptr.To[string](r.systemQOSCPUSet), nil
	}

	allSharePoolCPUs := make([]string, 0, len(r.sharePools))
	for _, nodeSharePool := range r.sharePools {
		allSharePoolCPUs = append(allSharePoolCPUs, nodeSharePool.CPUSet)
	}
	if podQOSClass == extension.QoSLS {
		// LS pods use all share pool
		klog.V(6).Infof("get cpuset from all share pool for container %v/%v",
			containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
		return ptr.To[string](strings.Join(allSharePoolCPUs, ",")), nil
	}

	kubeQOS := koordletutil.GetKubeQoSByCgroupParent(containerReq.CgroupParent)
	if kubeQOS == corev1.PodQOSBestEffort {
		// besteffort pods including QoS=BE, clear cpuset of BE container to avoid conflict with kubelet static policy,
		// which will pass cpuset in StartContainerRequest of CRI
		// TODO remove this in the future since cpu suppress will keep besteffort dir as all cpuset
		klog.V(6).Infof("get empty cpuset for be container %v/%v",
			containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
		return ptr.To[string](""), nil
	}

	if r.kubeletPolicy.Policy == extension.KubeletCPUManagerPolicyStatic {
		klog.V(6).Infof("get empty cpuset if kubelet is static policy for container %v/%v",
			containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
		return nil, nil
	} else {
		// none policy
		klog.V(6).Infof("get cpuset from all share pool if kubelet is none policy for container %v/%v",
			containerReq.PodMeta.String(), containerReq.ContainerMeta.Name)
		return ptr.To[string](strings.Join(allSharePoolCPUs, ",")), nil
	}
}

func (r *cpusetRule) getHostAppCpuset(hostAppReq *protocol.HostAppRequest) (*string, error) {
	if hostAppReq == nil {
		return nil, nil
	}
	if hostAppReq.QOSClass != extension.QoSLS {
		return nil, fmt.Errorf("only LS is supported for host application %v", hostAppReq.Name)
	}
	allSharePoolCPUs := make([]string, 0, len(r.sharePools))
	for _, nodeSharePool := range r.sharePools {
		allSharePoolCPUs = append(allSharePoolCPUs, nodeSharePool.CPUSet)
	}
	klog.V(6).Infof("get cpuset from all share pool for host application %v", hostAppReq.Name)
	return ptr.To[string](strings.Join(allSharePoolCPUs, ",")), nil
}

// getReservedCPUsFromNRT returns reserved CPUs from NRT annotations (node reservation + kubelet policy).
func getReservedCPUsFromNRT(annotations map[string]string) cpuset.CPUSet {
	reserved := cpuset.New()
	// From node.koordinator.sh/reservation (e.g. {"reservedCPUs":"0-1"})
	if s, _ := extension.GetReservedCPUs(annotations); s != "" {
		if c, err := cpuset.Parse(s); err == nil {
			reserved = reserved.Union(c)
		}
	}
	// From kubelet.koordinator.sh/cpu-manager-policy (when kubelet has reserved-cpus)
	if policy, err := extension.GetKubeletCPUManagerPolicy(annotations); err == nil && policy != nil && policy.ReservedCPUs != "" {
		if c, err := cpuset.Parse(policy.ReservedCPUs); err == nil {
			reserved = reserved.Union(c)
		}
	}
	return reserved
}

// tryCoerceNodeResourceTopology handles a duplicate Go type for *topology/v1alpha1.NodeResourceTopology (same
// import path pulled twice). Unrelated types are rejected (Elem().Name() must be NodeResourceTopology).
func tryCoerceNodeResourceTopology(nodeTopoIf interface{}) *topov1alpha1.NodeResourceTopology {
	if nodeTopoIf == nil {
		return nil
	}
	if nodeTopo, ok := nodeTopoIf.(*topov1alpha1.NodeResourceTopology); ok {
		return nodeTopo
	}
	rt := reflect.TypeOf(nodeTopoIf)
	if rt.Kind() != reflect.Ptr || rt.Elem().Name() != "NodeResourceTopology" {
		return nil
	}
	data, err := json.Marshal(nodeTopoIf)
	if err != nil {
		klog.V(4).Infof("coerce NodeResourceTopology: marshal failed: %v", err)
		return nil
	}
	var out topov1alpha1.NodeResourceTopology
	if err := json.Unmarshal(data, &out); err != nil {
		klog.V(4).Infof("coerce NodeResourceTopology: unmarshal failed: %v", err)
		return nil
	}
	klog.V(4).Infof("coerced NodeResourceTopology via JSON (%T -> *topov1alpha1.NodeResourceTopology)", nodeTopoIf)
	return &out
}

func (p *cpusetPlugin) parseRule(nodeTopoIf interface{}) (bool, error) {
	nodeTopo := tryCoerceNodeResourceTopology(nodeTopoIf)
	if nodeTopo == nil {
		return false, fmt.Errorf("parse format for hook plugin %v failed, expect: %v, got: %T",
			name, "*topov1alpha1.NodeResourceTopology", nodeTopoIf)
	}
	cpuSharePools, err := extension.GetNodeCPUSharePools(nodeTopo.Annotations)
	if err != nil {
		return false, err
	}
	// Debug: trace have-0 root cause
	cpuSharePoolsAnno := ""
	if nodeTopo.Annotations != nil {
		cpuSharePoolsAnno = nodeTopo.Annotations[extension.AnnotationNodeCPUSharedPools]
	}
	klog.V(4).Infof("m+n debug: cpuSharePools len=%d, annotation len=%d, raw=%q",
		len(cpuSharePools), len(cpuSharePoolsAnno), cpuSharePoolsAnno)
	beCPUSharePools, err := extension.GetNodeBECPUSharePools(nodeTopo.Annotations)
	if err != nil {
		return false, err
	}
	cpuManagerPolicy, err := extension.GetKubeletCPUManagerPolicy(nodeTopo.Annotations)
	if err != nil {
		return false, err
	}

	systemQOSCPUSet := ""
	systemQOSRes, err := extension.GetSystemQOSResource(nodeTopo.Annotations)
	if err != nil {
		return false, err
	} else if systemQOSRes != nil {
		// check cpuset format
		if _, err := cpuset.Parse(systemQOSRes.CPUSet); err != nil {
			return false, err
		} else {
			systemQOSCPUSet = systemQOSRes.CPUSet
		}
	}

	var shareCPUSetCount, beShareCPUSetCount int
	for _, nodeSharePool := range cpuSharePools {
		nodeSharePoolCPUSet, err := cpuset.Parse(nodeSharePool.CPUSet)
		if err != nil {
			klog.Errorf("failed to parse cpuset info of share pool, err: %v", err)
			continue
		}
		shareCPUSetCount += nodeSharePoolCPUSet.Size()
	}

	for _, nodeBESharePool := range beCPUSharePools {
		nodeBESharePoolCPUSet, err := cpuset.Parse(nodeBESharePool.CPUSet)
		if err != nil {
			klog.Errorf("failed to parse cpuset info of be share pool, err: %v", err)
			continue
		}
		beShareCPUSetCount += nodeBESharePoolCPUSet.Size()
	}

	metrics.RecordCPUSetSharePoolCores(float64(shareCPUSetCount))
	metrics.RecordCPUSetBESharePoolCores(float64(beShareCPUSetCount))

	var mPlusNAlloc mPlusNAllocatorInterface
	var strategy *extension.CPUExclusiveSharedStrategy
	var parsedStrategy *extension.ParsedCPUExclusiveSharedStrategy
	if parsed, ok := extension.ParseNodeCPUExclusiveSharedStrategy(nodeTopo.Annotations); ok {
		parsedStrategy = parsed
		if parsed.WholeNode != nil {
			strategy = parsed.WholeNode
		}
		extTopo, err := extension.GetCPUTopology(nodeTopo.Annotations)
		if err != nil {
			klog.V(4).Infof("m+n: failed to get cpu topology: %v", err)
		} else if extTopo != nil && len(extTopo.Detail) > 0 {
			topo := kubelet.NewCPUTopologyFromExtension(extTopo)
			allCPUList := make([]int, 0, len(extTopo.Detail))
			for _, c := range extTopo.Detail {
				allCPUList = append(allCPUList, int(c.ID))
			}
			allCPUsSet := cpuset.New(allCPUList...)

			reservedCPUs := getReservedCPUsFromNRT(nodeTopo.Annotations)
			allocatableCPUs := allCPUsSet
			if reservedCPUs.Size() > 0 {
				allocatableCPUs = allCPUsSet.Difference(reservedCPUs)
				klog.V(4).Infof("m+n: excluding reserved CPUs %v, allocatable %v", reservedCPUs.String(), allocatableCPUs.String())
			}

			checkpointPath := kubelet.GetCPUSetMPlusNStateFilePath(system.Conf.GetCpusetCheckpointRoot())

			if parsed.PerNUMA != nil {
				// Per-NUMA: build pools for each NUMA
				numaPools := make(map[int32]struct {
					DedicatedSet cpuset.CPUSet
					SharedSet    cpuset.CPUSet
					SharedPools  []cpuset.CPUSet
				})
				for numaID, strat := range parsed.PerNUMA {
					cpusInNUMANode := topo.CPUDetails.CPUsInNUMANodes(int(numaID)).Intersection(allocatableCPUs)
					if cpusInNUMANode.Size() == 0 {
						klog.V(4).Infof("m+n per-NUMA: NUMA %d has no allocatable CPUs, skip", numaID)
						continue
					}
					totalCPUs := cpusInNUMANode.Size()
					dedicatedTotal := int(strat.InstanceCap * strat.DedicatedCores)
					sharedTotal := totalCPUs - dedicatedTotal
					n := int(strat.SharedCores)

					var dedicatedSet, sharedSet cpuset.CPUSet
					var sharedPools []cpuset.CPUSet
					if dedicatedTotal > 0 && sharedTotal > 0 && n > 0 && sharedTotal >= n {
						dedicatedSet, err = kubelet.TakeByTopology(cpusInNUMANode, dedicatedTotal, topo)
						if err != nil {
							klog.V(4).Infof("m+n per-NUMA: TakeByTopology for NUMA %d failed: %v", numaID, err)
							continue
						}
						sharedSet = cpusInNUMANode.Difference(dedicatedSet)
						numPools := sharedTotal / n
						if numPools > 1 {
							remaining := sharedSet
							for i := 0; i < numPools && remaining.Size() >= n; i++ {
								pool, err := kubelet.TakeByTopology(remaining, n, topo)
								if err != nil {
									break
								}
								sharedPools = append(sharedPools, pool)
								remaining = remaining.Difference(pool)
							}
						}
					}
					if dedicatedSet.Size() > 0 {
						numaPools[numaID] = struct {
							DedicatedSet cpuset.CPUSet
							SharedSet    cpuset.CPUSet
							SharedPools  []cpuset.CPUSet
						}{DedicatedSet: dedicatedSet, SharedSet: sharedSet, SharedPools: sharedPools}
					}
				}
				if len(numaPools) > 0 {
					mPlusNAlloc = cpusetalloc.NewPerNUMAAllocator(numaPools, parsed.PerNUMA, topo, checkpointPath)
					klog.V(4).Infof("m+n per-NUMA allocator created: %d NUMA nodes", len(numaPools))
				}
			} else if strategy != nil {
				// Whole-node legacy
				strat := strategy
				totalCPUs := allocatableCPUs.Size()
				dedicatedTotal := int(strat.InstanceCap * strat.DedicatedCores)
				sharedTotal := totalCPUs - dedicatedTotal
				n := int(strat.SharedCores)

				var dedicatedSet, sharedSet cpuset.CPUSet
				var sharedPools []cpuset.CPUSet

				if dedicatedTotal > 0 && sharedTotal > 0 && n > 0 && sharedTotal >= n {
					dedicatedSet, err = kubelet.TakeByTopology(allocatableCPUs, dedicatedTotal, topo)
					if err != nil {
						klog.V(4).Infof("m+n: TakeByTopology for dedicated failed: %v", err)
					} else {
						sharedSet = allocatableCPUs.Difference(dedicatedSet)
						numPools := sharedTotal / n
						if numPools > 1 {
							remaining := sharedSet
							for i := 0; i < numPools && remaining.Size() >= n; i++ {
								pool, err := kubelet.TakeByTopology(remaining, n, topo)
								if err != nil {
									break
								}
								sharedPools = append(sharedPools, pool)
								remaining = remaining.Difference(pool)
							}
							klog.V(4).Infof("m+n: split shared into %d pools for round-robin", len(sharedPools))
						}
					}
				}

				if dedicatedSet.Size() == 0 {
					// Fallback: use cpuSharePools from NRT (LS share pool, excludes m+n dedicated)
					klog.V(4).Infof("m+n debug: TakeByTopology failed, fallback to cpuSharePools (len=%d)", len(cpuSharePools))
					sharedSet = cpuset.New()
					for _, pool := range cpuSharePools {
						poolSet, err := cpuset.Parse(pool.CPUSet)
						if err != nil {
							klog.V(4).Infof("m+n debug: failed to parse pool %v: %v", pool, err)
							continue
						}
						sharedSet = sharedSet.Union(poolSet)
					}
					klog.V(4).Infof("m+n debug: fallback sharedSet=%v, allocatableCPUs=%v", sharedSet.String(), allocatableCPUs.String())
					dedicatedSet = allocatableCPUs.Difference(sharedSet)
					// If cpuSharePools was empty (e.g. rule ran before NRT update), sharedSet is empty
					// and dedicatedSet=allocatableCPUs, causing allocator with empty sharedPool.
					// Retry TakeByTopology to compute dedicated+shared from allocatableCPUs.
					if sharedSet.Size() == 0 && dedicatedTotal > 0 && sharedTotal >= n {
						dedicatedSet, err = kubelet.TakeByTopology(allocatableCPUs, dedicatedTotal, topo)
						if err == nil && dedicatedSet.Size() > 0 {
							sharedSet = allocatableCPUs.Difference(dedicatedSet)
							klog.V(4).Infof("m+n fallback: recomputed dedicated %v, shared %v from TakeByTopology",
								dedicatedSet.String(), sharedSet.String())
						}
					}
				}

				// Do not create allocator when sharedSet is empty but n>0 (would fail with "have 0")
				// Use sharedPools mode when sharedSet has cores: single-pool mode "consumes" shared per pod
				// (availableShared = shared - allocated), so the 2nd+ pod gets "have 0". sharedPools mode
				// assigns the whole pool to each instance (round-robin), allowing multiple pods to share.
				if sharedSet.Size() > 0 && len(sharedPools) == 0 {
					sharedPools = []cpuset.CPUSet{sharedSet}
				}
				if dedicatedSet.Size() > 0 && (sharedSet.Size() > 0 || len(sharedPools) > 0 || n == 0) {
					if len(sharedPools) > 0 {
						mPlusNAlloc = cpusetalloc.NewAllocatorWithSharedPools(dedicatedSet, sharedPools, topo, checkpointPath)
						klog.V(4).Infof("m+n allocator created (multi-pool): dedicated %v, %d shared pools for round-robin",
							dedicatedSet.String(), len(sharedPools))
					} else {
						mPlusNAlloc = cpusetalloc.NewAllocator(dedicatedSet, sharedSet, topo, checkpointPath)
						klog.V(4).Infof("m+n allocator created: dedicated %v, shared %v", dedicatedSet.String(), sharedSet.String())
					}
				} else if dedicatedSet.Size() > 0 && sharedSet.Size() == 0 && n > 0 {
					klog.Warningf("m+n: skip creating allocator - sharedSet empty but need %d shared cores (allocatable %v, dedicated %v, cpuSharePools len=%d)",
						n, allocatableCPUs.String(), dedicatedSet.String(), len(cpuSharePools))
				}
			}
		}
	}

	releaseStaleFn := func() {}
	if mPlusNAlloc != nil && p.statesInformer != nil {
		releaseStaleFn = func() { p.releaseStaleForMPlusN() }
	}
	newRule := &cpusetRule{
		kubeletPolicy:               *cpuManagerPolicy,
		sharePools:                  cpuSharePools,
		beSharePools:                beCPUSharePools,
		systemQOSCPUSet:             systemQOSCPUSet,
		mPlusNAllocator:             mPlusNAlloc,
		strategy:                    strategy,
		parsedStrategy:              parsedStrategy,
		releaseStaleBeforeAllocate:  releaseStaleFn,
	}
	updated := p.updateRule(newRule)
	return updated, nil
}

// releaseStaleForMPlusN clears checkpoint entries for removed pods before allocating.
// Called from PreCreateContainer path where ruleUpdateCb may not have run yet.
func (p *cpusetPlugin) releaseStaleForMPlusN() {
	hasStrategy := func(r *cpusetRule) bool {
		return r != nil && (r.strategy != nil || (r.parsedStrategy != nil && r.parsedStrategy.PerNUMA != nil))
	}
	r := p.getRule()
	if r == nil || r.mPlusNAllocator == nil || !hasStrategy(r) {
		return
	}
	keepUIDs := make(map[types.UID]struct{})
	for _, podMeta := range p.statesInformer.GetAllPods() {
		if _, ok := extension.GetPodCPUExclusiveCores(podMeta.Pod.Annotations); ok {
			keepUIDs[types.UID(podMeta.Pod.UID)] = struct{}{}
		} else {
			qos := extension.GetQoSClassByAttrs(podMeta.Pod.Labels, podMeta.Pod.Annotations)
			if qos == extension.QoSLSR || qos == extension.QoSLSE {
				keepUIDs[types.UID(podMeta.Pod.UID)] = struct{}{}
			}
		}
	}
	r.mPlusNAllocator.ReleaseStale(keepUIDs)
}

func (p *cpusetPlugin) ruleUpdateCb(target *statesinformer.CallbackTarget) error {
	if target == nil {
		klog.Warningf("callback target is nil")
		return nil
	}
	// release m+n allocations for removed pods (annotation or LSR/LSE with strategy)
	hasStrategy := func(r *cpusetRule) bool {
		return r != nil && (r.strategy != nil || (r.parsedStrategy != nil && r.parsedStrategy.PerNUMA != nil))
	}
	if r := p.getRule(); r != nil && r.mPlusNAllocator != nil && hasStrategy(r) {
		keepUIDs := make(map[types.UID]struct{})
		for _, podMeta := range target.Pods {
			if _, ok := extension.GetPodCPUExclusiveCores(podMeta.Pod.Annotations); ok {
				keepUIDs[types.UID(podMeta.Pod.UID)] = struct{}{}
			} else {
				qos := extension.GetQoSClassByAttrs(podMeta.Pod.Labels, podMeta.Pod.Annotations)
				if qos == extension.QoSLSR || qos == extension.QoSLSE {
					keepUIDs[types.UID(podMeta.Pod.UID)] = struct{}{}
				}
			}
		}
		r.mPlusNAllocator.ReleaseStale(keepUIDs)
	}
	for _, podMeta := range target.Pods {
		allContainersSpec := make(map[string]*corev1.Container, len(podMeta.Pod.Spec.Containers)+len(podMeta.Pod.Spec.InitContainers))
		for i := range podMeta.Pod.Spec.InitContainers {
			initContainer := &podMeta.Pod.Spec.InitContainers[i]
			allContainersSpec[initContainer.Name] = initContainer
		}
		for i := range podMeta.Pod.Spec.Containers {
			container := &podMeta.Pod.Spec.Containers[i]
			allContainersSpec[container.Name] = container
		}

		allContainerStatus := make([]corev1.ContainerStatus, 0, len(podMeta.Pod.Status.ContainerStatuses)+len(podMeta.Pod.Status.InitContainerStatuses))
		allContainerStatus = append(allContainerStatus, podMeta.Pod.Status.ContainerStatuses...)
		allContainerStatus = append(allContainerStatus, podMeta.Pod.Status.InitContainerStatuses...)
		for _, containerStat := range allContainerStatus {
			containerSpec, exist := allContainersSpec[containerStat.Name]
			if !exist || containerSpec == nil {
				klog.Warningf("container %v not found in pod %v/%v, skip reconcile",
					containerStat.Name, podMeta.Pod.Namespace, podMeta.Pod.Name)
				continue
			}
			if protocol.ContainerReconcileIgnoreFilter(podMeta.Pod, containerSpec, &containerStat) {
				klog.V(5).Infof("container %v is ignored in pod %v/%v, skip reconcile",
					containerStat.Name, podMeta.Pod.Namespace, podMeta.Pod.Name)
				continue
			}

			containerCtx := &protocol.ContainerContext{}
			containerCtx.FromReconciler(podMeta, containerStat.Name, false)
			if err := p.SetContainerCPUSet(containerCtx); err != nil {
				klog.V(4).Infof("parse cpuset from pod annotation failed during callback, error: %v", err)
				continue
			}
			containerCtx.ReconcilerDone(p.executor)
		}

		sandboxContainerCtx := &protocol.ContainerContext{}
		sandboxContainerCtx.FromReconciler(podMeta, "", true)
		if err := p.SetContainerCPUSet(sandboxContainerCtx); err != nil {
			klog.Warningf("set cpuset for failed for pod sandbox %v/%v, error %v",
				sandboxContainerCtx.Request.PodMeta.String(), sandboxContainerCtx.Request.ContainerMeta.ID, err)
			continue
		}
		sandboxContainerCtx.ReconcilerDone(p.executor)
		klog.V(5).Infof("set cpuset finished pod sandbox %v/%v",
			sandboxContainerCtx.Request.PodMeta.String(), sandboxContainerCtx.Request.ContainerMeta.ID)
	}
	for _, hostApp := range target.HostApplications {
		hostCtx := protocol.HooksProtocolBuilder.HostApp(&hostApp)
		if err := p.SetHostAppCPUSet(hostCtx); err != nil {
			klog.Warningf("set host application %v cpuset value failed, error %v", hostApp.Name, err)
		} else {
			hostCtx.ReconcilerDone(p.executor)
			klog.V(5).Infof("set host application %v cpuset value finished", hostApp.Name)
		}
	}
	return nil
}

func (p *cpusetPlugin) getRule() *cpusetRule {
	p.ruleRWMutex.RLock()
	defer p.ruleRWMutex.RUnlock()
	if p.rule == nil {
		return nil
	}
	rule := *p.rule
	return &rule
}

func (p *cpusetPlugin) updateRule(newRule *cpusetRule) bool {
	p.ruleRWMutex.Lock()
	defer p.ruleRWMutex.Unlock()
	// Avoid reflect.DeepEqual on rule internals (maps/pointers/functions), which can
	// panic with "concurrent map read and map write" under async callback updates.
	// Replacing rule atomically is safe and keeps hook behavior deterministic.
	if p.rule == nil && newRule == nil {
		return false
	}
	p.rule = newRule
	return true
}
