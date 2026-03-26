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

package extension

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNodeCPUExclusiveSharedStrategy(t *testing.T) {
	tests := []struct {
		name     string
		annotations map[string]string
		wantOK   bool
		wantWhole *CPUExclusiveSharedStrategy
		wantPerNUMA map[int32]*CPUExclusiveSharedStrategy
	}{
		{
			name: "legacy whole-node",
			annotations: map[string]string{LabelNodeCPUExclusiveSharedStrategy: "20:4+8"},
			wantOK: true,
			wantWhole: &CPUExclusiveSharedStrategy{InstanceCap: 20, DedicatedCores: 4, SharedCores: 8},
		},
		{
			name: "per-NUMA single",
			annotations: map[string]string{LabelNodeCPUExclusiveSharedStrategy: "0:8:4+8"},
			wantOK: true,
			wantPerNUMA: map[int32]*CPUExclusiveSharedStrategy{
				0: {InstanceCap: 8, DedicatedCores: 4, SharedCores: 8},
			},
		},
		{
			name: "per-NUMA multi",
			annotations: map[string]string{LabelNodeCPUExclusiveSharedStrategy: "0:8:4+8,1:8:4+8"},
			wantOK: true,
			wantPerNUMA: map[int32]*CPUExclusiveSharedStrategy{
				0: {InstanceCap: 8, DedicatedCores: 4, SharedCores: 8},
				1: {InstanceCap: 8, DedicatedCores: 4, SharedCores: 8},
			},
		},
		{
			name: "per-NUMA different config",
			annotations: map[string]string{LabelNodeCPUExclusiveSharedStrategy: "0:4:2+4,1:8:4+8"},
			wantOK: true,
			wantPerNUMA: map[int32]*CPUExclusiveSharedStrategy{
				0: {InstanceCap: 4, DedicatedCores: 2, SharedCores: 4},
				1: {InstanceCap: 8, DedicatedCores: 4, SharedCores: 8},
			},
		},
		{
			name: "empty",
			annotations: map[string]string{},
			wantOK: false,
		},
		{
			name: "invalid legacy",
			annotations: map[string]string{LabelNodeCPUExclusiveSharedStrategy: "0:4+8"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseNodeCPUExclusiveSharedStrategy(tt.annotations)
			assert.Equal(t, tt.wantOK, ok)
			if !ok {
				return
			}
			if tt.wantWhole != nil {
				assert.NotNil(t, got.WholeNode)
				assert.Equal(t, tt.wantWhole.InstanceCap, got.WholeNode.InstanceCap)
				assert.Equal(t, tt.wantWhole.DedicatedCores, got.WholeNode.DedicatedCores)
				assert.Equal(t, tt.wantWhole.SharedCores, got.WholeNode.SharedCores)
			}
			if tt.wantPerNUMA != nil {
				assert.NotNil(t, got.PerNUMA)
				assert.Equal(t, len(tt.wantPerNUMA), len(got.PerNUMA))
				for numaID, want := range tt.wantPerNUMA {
					gotStrat := got.PerNUMA[numaID]
					assert.NotNil(t, gotStrat)
					assert.Equal(t, want.InstanceCap, gotStrat.InstanceCap)
					assert.Equal(t, want.DedicatedCores, gotStrat.DedicatedCores)
					assert.Equal(t, want.SharedCores, gotStrat.SharedCores)
				}
			}
		})
	}
}
