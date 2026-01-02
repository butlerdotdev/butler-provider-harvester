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

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	butlerv1alpha1 "github.com/butlerdotdev/butler-api/api/v1alpha1"
)

// GroupVersionResources for Harvester/KubeVirt resources.
var (
	vmGVR = schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachines",
	}

	vmiGVR = schema.GroupVersionResource{
		Group:    "kubevirt.io",
		Version:  "v1",
		Resource: "virtualmachineinstances",
	}
)

// Client provides access to Harvester resources.
type Client struct {
	dynamic   dynamic.Interface
	clientset *kubernetes.Clientset
	namespace string
	config    *butlerv1alpha1.HarvesterProviderConfig
}

// NewClient creates a new Harvester client from kubeconfig data.
func NewClient(kubeconfigData []byte, config *butlerv1alpha1.HarvesterProviderConfig) (*Client, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config: %w", err)
	}

	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	namespace := config.Namespace
	if namespace == "" {
		namespace = "default"
	}

	return &Client{
		dynamic:   dynamicClient,
		clientset: clientset,
		namespace: namespace,
		config:    config,
	}, nil
}

// VMCreateOptions defines options for creating a VM.
type VMCreateOptions struct {
	Name        string
	CPU         int32
	MemoryMB    int32
	DiskGB      int32
	ImageName   string // format: namespace/name
	NetworkName string // format: namespace/name
	UserData    string
	NetworkData string
	Labels      map[string]string
}

// CreateVM creates a new VirtualMachine in Harvester.
// This creates a PVC first (Harvester style), then the VM.
func (c *Client) CreateVM(ctx context.Context, opts VMCreateOptions) (string, error) {
	// Use image from options or fall back to config default
	imageName := opts.ImageName
	if imageName == "" {
		imageName = c.config.ImageName
	}
	if imageName == "" {
		return "", fmt.Errorf("no image specified and no default image in provider config")
	}

	// Use network from options or fall back to config
	networkName := opts.NetworkName
	if networkName == "" {
		networkName = c.config.NetworkName
	}

	// Create the PVC first (Harvester clones from image via StorageClass)
	pvcName := opts.Name + "-rootdisk"
	if err := c.createImagePVC(ctx, pvcName, imageName, opts.DiskGB); err != nil {
		return "", fmt.Errorf("failed to create PVC: %w", err)
	}

	// Build and create the VM
	vm := c.buildVM(opts, pvcName, networkName)

	created, err := c.dynamic.Resource(vmGVR).Namespace(c.namespace).Create(ctx, vm, metav1.CreateOptions{})
	if err != nil {
		// Clean up PVC if VM creation fails
		_ = c.clientset.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})
		return "", fmt.Errorf("failed to create VM: %w", err)
	}

	return string(created.GetUID()), nil
}

// createImagePVC creates a PVC that clones from a Harvester image.
func (c *Client) createImagePVC(ctx context.Context, name, imageName string, sizeGB int32) error {
	imageID := imageName // e.g., "default/image-prn78"
	storageClassName := fmt.Sprintf("longhorn-%s", parseName(imageName))

	blockMode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
			Annotations: map[string]string{
				"harvesterhci.io/imageId": imageID,
			},
			Labels: map[string]string{
				"butler.butlerlabs.dev/managed-by": "butler-provider-harvester",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			VolumeMode:       &blockMode,
			StorageClassName: &storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", sizeGB)),
				},
			},
		},
	}

	_, err := c.clientset.CoreV1().PersistentVolumeClaims(c.namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}

// buildVM constructs the VirtualMachine object.
func (c *Client) buildVM(opts VMCreateOptions, pvcName, networkName string) *unstructured.Unstructured {
	labels := map[string]interface{}{
		"butler.butlerlabs.dev/managed-by": "butler-provider-harvester",
	}
	for k, v := range opts.Labels {
		labels[k] = v
	}

	// Build volumes list
	volumes := []interface{}{
		map[string]interface{}{
			"name": "rootdisk",
			"persistentVolumeClaim": map[string]interface{}{
				"claimName": pvcName,
			},
		},
	}

	// Build disks list
	disks := []interface{}{
		map[string]interface{}{
			"name":      "rootdisk",
			"bootOrder": int64(1),
			"disk": map[string]interface{}{
				"bus": "virtio",
			},
		},
	}

	// Add cloud-init if userData is provided
	if opts.UserData != "" {
		cloudInitVolume := map[string]interface{}{
			"name": "cloudinit",
			"cloudInitNoCloud": map[string]interface{}{
				"userDataBase64": base64.StdEncoding.EncodeToString([]byte(opts.UserData)),
			},
		}
		if opts.NetworkData != "" {
			cloudInitVolume["cloudInitNoCloud"].(map[string]interface{})["networkDataBase64"] = base64.StdEncoding.EncodeToString([]byte(opts.NetworkData))
		}
		volumes = append(volumes, cloudInitVolume)
		disks = append(disks, map[string]interface{}{
			"name": "cloudinit",
			"disk": map[string]interface{}{
				"bus": "virtio",
			},
		})
	}

	vm := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kubevirt.io/v1",
			"kind":       "VirtualMachine",
			"metadata": map[string]interface{}{
				"name":      opts.Name,
				"namespace": c.namespace,
				"labels":    labels,
				"annotations": map[string]interface{}{
					"harvesterhci.io/vmRunStrategy": "Always",
				},
			},
			"spec": map[string]interface{}{
				"runStrategy": "Always",
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{
						"labels": labels,
					},
					"spec": map[string]interface{}{
						"domain": map[string]interface{}{
							"cpu": map[string]interface{}{
								"cores":   int64(opts.CPU),
								"sockets": int64(1),
								"threads": int64(1),
							},
							"memory": map[string]interface{}{
								"guest": fmt.Sprintf("%dMi", opts.MemoryMB),
							},
							"resources": map[string]interface{}{
								"limits": map[string]interface{}{
									"cpu":    fmt.Sprintf("%d", opts.CPU),
									"memory": fmt.Sprintf("%dMi", opts.MemoryMB),
								},
								"requests": map[string]interface{}{
									"cpu":    "125m",
									"memory": fmt.Sprintf("%dMi", opts.MemoryMB),
								},
							},
							"devices": map[string]interface{}{
								"disks": disks,
								"interfaces": []interface{}{
									map[string]interface{}{
										"name":   "default",
										"bridge": map[string]interface{}{},
									},
								},
							},
						},
						"networks": []interface{}{
							map[string]interface{}{
								"name": "default",
								"multus": map[string]interface{}{
									"networkName": networkName,
								},
							},
						},
						"volumes": volumes,
					},
				},
			},
		},
	}

	return vm
}

// GetVM retrieves a VirtualMachine by name.
func (c *Client) GetVM(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return c.dynamic.Resource(vmGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
}

// GetVMI retrieves a VirtualMachineInstance by name.
func (c *Client) GetVMI(ctx context.Context, name string) (*unstructured.Unstructured, error) {
	return c.dynamic.Resource(vmiGVR).Namespace(c.namespace).Get(ctx, name, metav1.GetOptions{})
}

// DeleteVM deletes a VirtualMachine and its associated PVC.
func (c *Client) DeleteVM(ctx context.Context, name string) error {
	// Delete the VM first
	err := c.dynamic.Resource(vmGVR).Namespace(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return err
	}

	// Delete the associated PVC
	pvcName := name + "-rootdisk"
	_ = c.clientset.CoreV1().PersistentVolumeClaims(c.namespace).Delete(ctx, pvcName, metav1.DeleteOptions{})

	return nil
}

// VMStatus represents the status of a VM.
type VMStatus struct {
	Exists     bool
	Ready      bool
	Phase      string
	IPAddress  string
	MACAddress string
}

// GetVMStatus returns the current status of a VM.
func (c *Client) GetVMStatus(ctx context.Context, name string) (*VMStatus, error) {
	status := &VMStatus{}

	// Check if VM exists
	vm, err := c.GetVM(ctx, name)
	if err != nil {
		return status, err
	}
	status.Exists = true

	// Get VM ready status
	ready, found, _ := unstructured.NestedBool(vm.Object, "status", "ready")
	if found {
		status.Ready = ready
	}

	printableStatus, _, _ := unstructured.NestedString(vm.Object, "status", "printableStatus")
	status.Phase = printableStatus

	// Get VMI for IP address
	vmi, err := c.GetVMI(ctx, name)
	if err != nil {
		// VMI might not exist yet if VM is still starting
		return status, nil
	}

	// Extract IP from VMI interfaces
	interfaces, found, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
	if found && len(interfaces) > 0 {
		for _, iface := range interfaces {
			ifaceMap, ok := iface.(map[string]interface{})
			if !ok {
				continue
			}
			ip, _, _ := unstructured.NestedString(ifaceMap, "ipAddress")
			if ip != "" && isUsableIP(ip) {
				status.IPAddress = ip
				mac, _, _ := unstructured.NestedString(ifaceMap, "mac")
				status.MACAddress = mac
				break
			}
		}
	}

	return status, nil
}

// parseName extracts name from "namespace/name" format.
func parseName(ref string) string {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}
	return ref
}

// isUsableIP returns true if the IP is a routable IPv4 address
func isUsableIP(ip string) bool {
	// Skip IPv6 (contains colons)
	if strings.Contains(ip, ":") {
		return false
	}
	// Skip IPv4 link-local (169.254.x.x)
	if strings.HasPrefix(ip, "169.254.") {
		return false
	}
	return true
}
