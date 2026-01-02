/*
Copyright 2026 The Butler Authors.

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

package harvester

// Harvester/KubeVirt constants.
const (
	// VirtualMachineAPIVersion is the API version for Harvester VMs.
	VirtualMachineAPIVersion = "kubevirt.io/v1"
	// VirtualMachineKind is the kind for VirtualMachine resources.
	VirtualMachineKind = "VirtualMachine"
	// VirtualMachineResource is the resource name for VirtualMachines.
	VirtualMachineResource = "virtualmachines"

	// DataVolumeAPIVersion is the API version for DataVolumes.
	DataVolumeAPIVersion = "cdi.kubevirt.io/v1beta1"
	// DataVolumeKind is the kind for DataVolume resources.
	DataVolumeKind = "DataVolume"

	// Harvester-specific annotations.
	AnnotationNetworkIPs  = "networks.harvesterhci.io/ips"
	AnnotationSSHNames    = "harvesterhci.io/sshNames"
	AnnotationCloudInit   = "harvesterhci.io/cloud-init-user-data"
	AnnotationNetworkData = "harvesterhci.io/cloud-init-network-data"

	// Harvester network annotation for VM networks.
	AnnotationNetworks = "k8s.v1.cni.cncf.io/networks"
)
