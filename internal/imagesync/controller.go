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

// Package imagesync provides ImageSync fulfillment for Harvester providers
// during bootstrap. This controller watches ImageSync resources and creates
// VirtualMachineImages on the target Harvester cluster.
//
// NOTE: This code is duplicated from butler-controller/internal/controller/imagesync/
// because during bootstrap, only provider controllers are running (butler-controller
// isn't installed yet). In steady state, butler-controller handles ImageSync.
// Bug fixes must be applied to both locations until a shared module is extracted.
package imagesync

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	butlerv1alpha1 "github.com/butlerdotdev/butler-api/api/v1alpha1"
)

const (
	// Requeue intervals.
	requeueShort = 15 * time.Second
	requeueLong  = 60 * time.Second
	requeueReady = 10 * time.Minute
)

var vmiImageGVR = schema.GroupVersionResource{
	Group:    "harvesterhci.io",
	Version:  "v1beta1",
	Resource: "virtualmachineimages",
}

// Reconciler reconciles ImageSync resources for Harvester providers.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=imagesyncs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=imagesyncs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=imagesyncs/finalizers,verbs=update
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=providerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=butlerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	is := &butlerv1alpha1.ImageSync{}
	if err := r.Get(ctx, req.NamespacedName, is); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciling ImageSync", "name", is.Name, "phase", is.Status.Phase)

	// Get ProviderConfig to check if this is a Harvester request
	pc, err := r.getProviderConfig(ctx, is)
	if err != nil {
		// Can't determine provider — skip silently (may be for another provider)
		logger.V(1).Info("skipping ImageSync: cannot get ProviderConfig", "error", err)
		return ctrl.Result{}, nil
	}

	// Only handle Harvester provider requests
	if pc.Spec.Provider != butlerv1alpha1.ProviderTypeHarvester {
		logger.V(1).Info("skipping non-Harvester ImageSync", "provider", pc.Spec.Provider)
		return ctrl.Result{}, nil
	}

	// Handle deletion
	if !is.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, is)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(is, butlerv1alpha1.FinalizerImageSync) {
		controllerutil.AddFinalizer(is, butlerv1alpha1.FinalizerImageSync)
		if err := r.Update(ctx, is); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set initial phase if not set
	if is.Status.Phase == "" {
		is.SetPhase(butlerv1alpha1.ImageSyncPhasePending)
		is.Status.ObservedGeneration = is.Generation
		if err := r.Status().Update(ctx, is); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Fetch ButlerConfig for factory URL
	bc, err := r.getButlerConfig(ctx)
	if err != nil {
		return r.setFailed(ctx, is, "ButlerConfigNotFound", err.Error())
	}

	if !bc.IsImageFactoryConfigured() {
		return r.setFailed(ctx, is, "ImageFactoryNotConfigured", "ButlerConfig.spec.imageFactory is not configured")
	}

	// Dispatch based on phase
	switch is.Status.Phase {
	case butlerv1alpha1.ImageSyncPhasePending:
		return r.reconcilePending(ctx, is, pc, bc)
	case butlerv1alpha1.ImageSyncPhaseDownloading, butlerv1alpha1.ImageSyncPhaseUploading:
		return r.reconcileInProgress(ctx, is, pc)
	case butlerv1alpha1.ImageSyncPhaseFailed:
		return ctrl.Result{RequeueAfter: requeueLong}, nil
	case butlerv1alpha1.ImageSyncPhaseReady:
		return ctrl.Result{RequeueAfter: requeueReady}, nil
	}

	return ctrl.Result{}, nil
}

// reconcilePending initiates the Harvester image sync.
func (r *Reconciler) reconcilePending(ctx context.Context, is *butlerv1alpha1.ImageSync, pc *butlerv1alpha1.ProviderConfig, bc *butlerv1alpha1.ButlerConfig) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	factoryURL := bc.GetImageFactoryURL()
	artifactURL := buildArtifactURL(factoryURL, is.Spec.FactoryRef, is.Spec.Format)
	imageName := buildProviderImageName(is)

	// Store artifact URL in status
	is.Status.ArtifactURL = artifactURL

	// Get Harvester credentials
	creds, err := r.getProviderCredentials(ctx, pc)
	if err != nil {
		return r.setFailed(ctx, is, "CredentialsError", fmt.Sprintf("failed to get Harvester credentials: %v", err))
	}

	kubeconfigData, ok := creds["kubeconfig"]
	if !ok {
		return r.setFailed(ctx, is, "CredentialsError", "Harvester credentials secret missing 'kubeconfig' key")
	}

	dynClient, err := newHarvesterDynamicClient(kubeconfigData)
	if err != nil {
		return r.setFailed(ctx, is, "ClientError", fmt.Sprintf("failed to create Harvester client: %v", err))
	}

	namespace := "default"
	if pc.Spec.Harvester != nil && pc.Spec.Harvester.Namespace != "" {
		namespace = pc.Spec.Harvester.Namespace
	}

	// Check if VMI already exists
	existing, err := dynClient.Resource(vmiImageGVR).Namespace(namespace).Get(ctx, imageName, metav1.GetOptions{})
	if err == nil {
		// VMI exists — check its status
		return r.checkVMIStatus(ctx, is, existing, namespace, imageName)
	}

	// VMI doesn't exist — create it
	if is.Spec.TransferMode == butlerv1alpha1.TransferModeProxy {
		return r.setFailed(ctx, is, "UnsupportedTransferMode",
			"proxy transfer mode for Harvester is not yet implemented; use direct mode")
	}

	logger.Info("creating Harvester VirtualMachineImage", "name", imageName, "url", artifactURL)

	vmi := buildHarvesterVMI(imageName, namespace, artifactURL, is.Spec.DisplayName)
	_, err = dynClient.Resource(vmiImageGVR).Namespace(namespace).Create(ctx, vmi, metav1.CreateOptions{})
	if err != nil {
		return r.setFailed(ctx, is, "CreateImageFailed", fmt.Sprintf("failed to create VirtualMachineImage: %v", err))
	}

	// Transition to Downloading (Harvester CDI pulls the image)
	is.SetPhase(butlerv1alpha1.ImageSyncPhaseDownloading)
	is.Status.ObservedGeneration = is.Generation
	meta.SetStatusCondition(&is.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             butlerv1alpha1.ReasonImageDownloading,
		Message:            fmt.Sprintf("Harvester is downloading image from %s", artifactURL),
		ObservedGeneration: is.Generation,
	})
	if err := r.Status().Update(ctx, is); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

// reconcileInProgress polls the status of a Harvester VirtualMachineImage.
func (r *Reconciler) reconcileInProgress(ctx context.Context, is *butlerv1alpha1.ImageSync, pc *butlerv1alpha1.ProviderConfig) (ctrl.Result, error) {
	imageName := buildProviderImageName(is)

	creds, err := r.getProviderCredentials(ctx, pc)
	if err != nil {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	kubeconfigData, ok := creds["kubeconfig"]
	if !ok {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	dynClient, err := newHarvesterDynamicClient(kubeconfigData)
	if err != nil {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	namespace := "default"
	if pc.Spec.Harvester != nil && pc.Spec.Harvester.Namespace != "" {
		namespace = pc.Spec.Harvester.Namespace
	}

	vmi, err := dynClient.Resource(vmiImageGVR).Namespace(namespace).Get(ctx, imageName, metav1.GetOptions{})
	if err != nil {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	return r.checkVMIStatus(ctx, is, vmi, namespace, imageName)
}

// checkVMIStatus inspects VMI conditions and transitions phases.
func (r *Reconciler) checkVMIStatus(ctx context.Context, is *butlerv1alpha1.ImageSync, vmi *unstructured.Unstructured, namespace, imageName string) (ctrl.Result, error) {
	conditions, found, _ := unstructured.NestedSlice(vmi.Object, "status", "conditions")
	if !found {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	for _, c := range conditions {
		condMap, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		condType, _, _ := unstructured.NestedString(condMap, "type")
		condStatus, _, _ := unstructured.NestedString(condMap, "status")
		message, _, _ := unstructured.NestedString(condMap, "message")

		if condType == "Imported" && condStatus == "True" {
			providerRef := fmt.Sprintf("%s/%s", namespace, imageName)
			return r.setReady(ctx, is, providerRef)
		}

		if condType == "Imported" && condStatus == "False" {
			reason, _, _ := unstructured.NestedString(condMap, "reason")
			if reason == "ImportFailed" || reason == "Failed" {
				return r.setFailed(ctx, is, "HarvesterImportFailed",
					fmt.Sprintf("VirtualMachineImage import failed: %s", message))
			}
		}

		if condType == "RetryLimitExceeded" && condStatus == "True" {
			return r.setFailed(ctx, is, "HarvesterRetryLimitExceeded",
				fmt.Sprintf("VirtualMachineImage download failed after retries: %s", message))
		}

		if condType == "Initialized" && condStatus == "False" {
			reason, _, _ := unstructured.NestedString(condMap, "reason")
			if reason == "Failed" || reason == "ImportFailed" {
				return r.setFailed(ctx, is, "HarvesterInitFailed",
					fmt.Sprintf("VirtualMachineImage initialization failed: %s", message))
			}
		}
	}

	// Still importing
	if is.Status.Phase == butlerv1alpha1.ImageSyncPhasePending {
		is.SetPhase(butlerv1alpha1.ImageSyncPhaseDownloading)
		is.Status.ObservedGeneration = is.Generation
		if err := r.Status().Update(ctx, is); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

// handleDeletion removes the finalizer.
func (r *Reconciler) handleDeletion(ctx context.Context, is *butlerv1alpha1.ImageSync) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(is, butlerv1alpha1.FinalizerImageSync) {
		return ctrl.Result{}, nil
	}

	logger.Info("removing ImageSync finalizer", "name", is.Name)

	if err := r.Get(ctx, types.NamespacedName{Name: is.Name, Namespace: is.Namespace}, is); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(is, butlerv1alpha1.FinalizerImageSync)
	return ctrl.Result{}, r.Update(ctx, is)
}

// setFailed sets the ImageSync to Failed phase.
func (r *Reconciler) setFailed(ctx context.Context, is *butlerv1alpha1.ImageSync, reason, message string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Error(fmt.Errorf("%s", message), "image sync failed", "reason", reason)

	is.SetFailure(reason, message)
	is.Status.ObservedGeneration = is.Generation
	meta.SetStatusCondition(&is.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: is.Generation,
	})

	if err := r.Status().Update(ctx, is); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// setReady transitions the ImageSync to Ready phase.
func (r *Reconciler) setReady(ctx context.Context, is *butlerv1alpha1.ImageSync, providerImageRef string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("image sync ready", "providerImageRef", providerImageRef)

	is.SetPhase(butlerv1alpha1.ImageSyncPhaseReady)
	is.Status.ProviderImageRef = providerImageRef
	is.Status.ObservedGeneration = is.Generation
	is.Status.FailureReason = ""
	is.Status.FailureMessage = ""
	meta.SetStatusCondition(&is.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionTrue,
		Reason:             butlerv1alpha1.ReasonImageReady,
		Message:            fmt.Sprintf("Image synced to provider: %s", providerImageRef),
		ObservedGeneration: is.Generation,
	})

	if err := r.Status().Update(ctx, is); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueReady}, nil
}

// Helper methods

func (r *Reconciler) getProviderConfig(ctx context.Context, is *butlerv1alpha1.ImageSync) (*butlerv1alpha1.ProviderConfig, error) {
	pc := &butlerv1alpha1.ProviderConfig{}
	ns := is.Spec.ProviderConfigRef.Namespace
	if ns == "" {
		ns = is.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      is.Spec.ProviderConfigRef.Name,
		Namespace: ns,
	}, pc); err != nil {
		return nil, fmt.Errorf("failed to get ProviderConfig %s/%s: %w", ns, is.Spec.ProviderConfigRef.Name, err)
	}
	return pc, nil
}

func (r *Reconciler) getButlerConfig(ctx context.Context) (*butlerv1alpha1.ButlerConfig, error) {
	bc := &butlerv1alpha1.ButlerConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: "butler"}, bc); err != nil {
		return nil, fmt.Errorf("failed to get ButlerConfig: %w", err)
	}
	return bc, nil
}

func (r *Reconciler) getProviderCredentials(ctx context.Context, pc *butlerv1alpha1.ProviderConfig) (map[string][]byte, error) {
	ns := pc.Spec.CredentialsRef.Namespace
	if ns == "" {
		ns = pc.Namespace
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: pc.Spec.CredentialsRef.Name, Namespace: ns}, secret); err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", ns, pc.Spec.CredentialsRef.Name, err)
	}
	return secret.Data, nil
}

// buildArtifactURL constructs the factory download URL for an image.
// URL format: {factory}/image/{schematic}/{version}/{platform}-{arch}.{format}
func buildArtifactURL(factoryURL string, ref butlerv1alpha1.ImageFactoryRef, format string) string {
	factoryURL = strings.TrimSuffix(factoryURL, "/")
	if format == "" {
		format = "qcow2"
	}
	arch := ref.Arch
	if arch == "" {
		arch = "amd64"
	}
	platform := ref.Platform
	if platform == "" {
		platform = "nocloud"
	}
	return fmt.Sprintf("%s/image/%s/%s/%s-%s.%s", factoryURL, ref.SchematicID, ref.Version, platform, arch, format)
}

// buildProviderImageName generates a deterministic image name for the provider.
func buildProviderImageName(is *butlerv1alpha1.ImageSync) string {
	if is.Spec.DisplayName != "" {
		return sanitizeName(is.Spec.DisplayName)
	}
	platform := is.Spec.FactoryRef.Platform
	if platform == "" {
		platform = "nocloud"
	}
	version := strings.ReplaceAll(is.Spec.FactoryRef.Version, ".", "-")
	arch := is.Spec.FactoryRef.Arch
	if arch == "" {
		arch = "amd64"
	}
	schematicPrefix := is.Spec.FactoryRef.SchematicID
	if len(schematicPrefix) > 8 {
		schematicPrefix = schematicPrefix[:8]
	}
	name := fmt.Sprintf("%s-%s-%s-%s-butler", platform, version, arch, schematicPrefix)
	return sanitizeName(name)
}

// invalidDNSChars matches any character not allowed in DNS subdomain names.
var invalidDNSChars = regexp.MustCompile(`[^a-z0-9.-]`)

// sanitizeName converts a string into a valid Kubernetes DNS subdomain name.
func sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.ReplaceAll(name, "_", "-")
	name = invalidDNSChars.ReplaceAllString(name, "")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	if len(name) > 63 {
		name = name[:63]
	}
	name = strings.Trim(name, "-.")
	return name
}

// buildHarvesterVMI constructs a VirtualMachineImage with sourceType=download.
func buildHarvesterVMI(name, namespace, downloadURL, displayName string) *unstructured.Unstructured {
	if displayName == "" {
		displayName = name
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "harvesterhci.io/v1beta1",
			"kind":       "VirtualMachineImage",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "butler",
				},
				"annotations": map[string]interface{}{
					"harvesterhci.io/image-download-url": downloadURL,
				},
			},
			"spec": map[string]interface{}{
				"displayName": displayName,
				"sourceType":  "download",
				"url":         downloadURL,
			},
		},
	}
}

// newHarvesterDynamicClient creates a dynamic client for a Harvester cluster.
func newHarvesterDynamicClient(kubeconfigData []byte) (dynamic.Interface, error) {
	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("failed to create REST config: %w", err)
	}
	return dynamic.NewForConfig(restConfig)
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&butlerv1alpha1.ImageSync{}).
		Named("imagesync").
		Complete(r)
}
