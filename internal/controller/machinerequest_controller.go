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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	butlerv1alpha1 "github.com/butlerdotdev/butler-api/api/v1alpha1"
	"github.com/butlerdotdev/butler-provider-harvester/internal/harvester"
)

const (
	finalizerName = "machinerequest.butler.butlerlabs.dev/harvester-finalizer"

	// Requeue intervals.
	requeueShort = 10 * time.Second
	requeueLong  = 30 * time.Second
)

// MachineRequestReconciler reconciles a MachineRequest object
type MachineRequestReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=machinerequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=machinerequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=machinerequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=butler.butlerlabs.dev,resources=providerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles MachineRequest reconciliation.
func (r *MachineRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the MachineRequest
	machineRequest := &butlerv1alpha1.MachineRequest{}
	if err := r.Get(ctx, req.NamespacedName, machineRequest); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get the ProviderConfig to check if this is a Harvester request
	providerConfig, err := r.getProviderConfig(ctx, machineRequest)
	if err != nil {
		log.Error(err, "Failed to get ProviderConfig")
		return r.updateStatusError(ctx, machineRequest, "ProviderConfigError", err.Error())
	}

	// Only handle Harvester provider requests
	if providerConfig.Spec.Provider != butlerv1alpha1.ProviderTypeHarvester {
		log.V(1).Info("Skipping non-Harvester MachineRequest", "provider", providerConfig.Spec.Provider)
		return ctrl.Result{}, nil
	}

	// Create Harvester client
	harvesterClient, err := r.createHarvesterClient(ctx, providerConfig)
	if err != nil {
		log.Error(err, "Failed to create Harvester client")
		return r.updateStatusError(ctx, machineRequest, "HarvesterClientError", err.Error())
	}

	// Handle deletion
	if !machineRequest.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, machineRequest, harvesterClient)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(machineRequest, finalizerName) {
		controllerutil.AddFinalizer(machineRequest, finalizerName)
		if err := r.Update(ctx, machineRequest); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Reconcile based on current phase
	switch machineRequest.Status.Phase {
	case "", butlerv1alpha1.MachinePhasePending:
		return r.reconcilePending(ctx, machineRequest, harvesterClient)
	case butlerv1alpha1.MachinePhaseCreating:
		return r.reconcileCreating(ctx, machineRequest, harvesterClient)
	case butlerv1alpha1.MachinePhaseRunning:
		return r.reconcileRunning(ctx, machineRequest, harvesterClient)
	case butlerv1alpha1.MachinePhaseFailed:
		// Don't reconcile failed machines unless manually reset
		return ctrl.Result{}, nil
	default:
		log.Info("Unknown phase, resetting to Pending", "phase", machineRequest.Status.Phase)
		return r.updatePhase(ctx, machineRequest, butlerv1alpha1.MachinePhasePending)
	}
}

// reconcilePending handles the Pending phase - creates the VM.
func (r *MachineRequestReconciler) reconcilePending(
	ctx context.Context,
	mr *butlerv1alpha1.MachineRequest,
	hc *harvester.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Creating VM", "name", mr.Spec.MachineName)

	opts := harvester.VMCreateOptions{
		Name:        mr.Spec.MachineName,
		CPU:         mr.Spec.CPU,
		MemoryMB:    mr.Spec.MemoryMB,
		DiskGB:      mr.Spec.DiskGB,
		ImageName:   mr.Spec.Image,
		UserData:    mr.Spec.UserData,
		NetworkData: mr.Spec.NetworkData,
		Labels:      mr.Spec.Labels,
	}

	providerID, err := hc.CreateVM(ctx, opts)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// VM already exists, move to Creating phase to check status
			log.Info("VM already exists, checking status")
			return r.updatePhase(ctx, mr, butlerv1alpha1.MachinePhaseCreating)
		}
		log.Error(err, "Failed to create VM")
		r.Recorder.Eventf(mr, corev1.EventTypeWarning, "CreateFailed", "Failed to create VM: %v", err)
		return r.updateStatusError(ctx, mr, butlerv1alpha1.ReasonProviderError, err.Error())
	}

	// Update status with provider ID and move to Creating phase
	mr.Status.ProviderID = providerID
	mr.Status.Phase = butlerv1alpha1.MachinePhaseCreating
	mr.Status.FailureReason = ""
	mr.Status.FailureMessage = ""
	now := metav1.Now()
	mr.Status.LastUpdated = &now
	mr.Status.ObservedGeneration = mr.Generation

	meta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             butlerv1alpha1.ReasonCreating,
		Message:            "VM is being created",
		ObservedGeneration: mr.Generation,
	})

	if err := r.Status().Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	r.Recorder.Event(mr, corev1.EventTypeNormal, "Created", "VM creation initiated")
	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

// reconcileCreating handles the Creating phase - waits for IP.
func (r *MachineRequestReconciler) reconcileCreating(
	ctx context.Context,
	mr *butlerv1alpha1.MachineRequest,
	hc *harvester.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Checking VM status", "name", mr.Spec.MachineName)

	status, err := hc.GetVMStatus(ctx, mr.Spec.MachineName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// VM doesn't exist, go back to Pending to recreate
			log.Info("VM not found, returning to Pending phase")
			return r.updatePhase(ctx, mr, butlerv1alpha1.MachinePhasePending)
		}
		log.Error(err, "Failed to get VM status")
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}

	log.V(1).Info("VM status", "ready", status.Ready, "phase", status.Phase, "ip", status.IPAddress)

	// Check if we have an IP address
	if status.IPAddress != "" {
		log.Info("VM is ready", "ip", status.IPAddress)

		mr.Status.Phase = butlerv1alpha1.MachinePhaseRunning
		mr.Status.IPAddress = status.IPAddress
		mr.Status.MACAddress = status.MACAddress
		now := metav1.Now()
		mr.Status.LastUpdated = &now

		meta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
			Type:               butlerv1alpha1.ConditionTypeReady,
			Status:             metav1.ConditionTrue,
			Reason:             butlerv1alpha1.ReasonRunning,
			Message:            fmt.Sprintf("VM is running with IP %s", status.IPAddress),
			ObservedGeneration: mr.Generation,
		})
		meta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
			Type:               butlerv1alpha1.ConditionTypeProgressing,
			Status:             metav1.ConditionFalse,
			Reason:             butlerv1alpha1.ReasonRunning,
			Message:            "VM creation complete",
			ObservedGeneration: mr.Generation,
		})

		if err := r.Status().Update(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}

		r.Recorder.Eventf(mr, corev1.EventTypeNormal, "Ready", "VM is running with IP %s", status.IPAddress)
		return ctrl.Result{}, nil
	}

	// Still waiting for IP, update condition and requeue
	meta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             butlerv1alpha1.ReasonWaitingForIP,
		Message:            fmt.Sprintf("VM phase: %s, waiting for IP address", status.Phase),
		ObservedGeneration: mr.Generation,
	})

	if err := r.Status().Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueShort}, nil
}

// reconcileRunning handles the Running phase - monitors for drift.
func (r *MachineRequestReconciler) reconcileRunning(
	ctx context.Context,
	mr *butlerv1alpha1.MachineRequest,
	hc *harvester.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Periodically verify the VM still exists and is running
	status, err := hc.GetVMStatus(ctx, mr.Spec.MachineName)
	if err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("VM no longer exists, marking as failed")
			mr.SetFailure("VMDeleted", "VM was deleted externally")
			if err := r.Status().Update(ctx, mr); err != nil {
				return ctrl.Result{}, err
			}
			r.Recorder.Event(mr, corev1.EventTypeWarning, "VMDeleted", "VM was deleted externally")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: requeueLong}, nil
	}

	// Update IP if it changed
	if status.IPAddress != "" && status.IPAddress != mr.Status.IPAddress {
		log.Info("VM IP changed", "old", mr.Status.IPAddress, "new", status.IPAddress)
		mr.Status.IPAddress = status.IPAddress
		now := metav1.Now()
		mr.Status.LastUpdated = &now
		if err := r.Status().Update(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: requeueLong}, nil
}

// reconcileDelete handles VM deletion.
func (r *MachineRequestReconciler) reconcileDelete(
	ctx context.Context,
	mr *butlerv1alpha1.MachineRequest,
	hc *harvester.Client,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("Deleting VM", "name", mr.Spec.MachineName)

	// Update phase to Deleting
	if mr.Status.Phase != butlerv1alpha1.MachinePhaseDeleting {
		mr.Status.Phase = butlerv1alpha1.MachinePhaseDeleting
		now := metav1.Now()
		mr.Status.LastUpdated = &now
		if err := r.Status().Update(ctx, mr); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Delete the VM
	if err := hc.DeleteVM(ctx, mr.Spec.MachineName); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "Failed to delete VM")
			return ctrl.Result{RequeueAfter: requeueShort}, nil
		}
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(mr, finalizerName)
	if err := r.Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("VM deleted successfully")
	r.Recorder.Event(mr, corev1.EventTypeNormal, "Deleted", "VM deleted")
	return ctrl.Result{}, nil
}

// Helper methods

func (r *MachineRequestReconciler) getProviderConfig(ctx context.Context, mr *butlerv1alpha1.MachineRequest) (*butlerv1alpha1.ProviderConfig, error) {
	pc := &butlerv1alpha1.ProviderConfig{}
	ns := mr.Spec.ProviderRef.Namespace
	if ns == "" {
		ns = mr.Namespace
	}

	key := types.NamespacedName{
		Name:      mr.Spec.ProviderRef.Name,
		Namespace: ns,
	}

	if err := r.Get(ctx, key, pc); err != nil {
		return nil, fmt.Errorf("failed to get ProviderConfig %s: %w", key, err)
	}

	return pc, nil
}

func (r *MachineRequestReconciler) createHarvesterClient(ctx context.Context, pc *butlerv1alpha1.ProviderConfig) (*harvester.Client, error) {
	if pc.Spec.Harvester == nil {
		return nil, fmt.Errorf("ProviderConfig %s has no Harvester configuration", pc.Name)
	}

	// Get credentials secret
	secret := &corev1.Secret{}
	ns := pc.Spec.CredentialsRef.Namespace
	if ns == "" {
		ns = pc.Namespace
	}

	key := types.NamespacedName{
		Name:      pc.Spec.CredentialsRef.Name,
		Namespace: ns,
	}

	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret %s: %w", key, err)
	}

	// Get kubeconfig from secret
	secretKey := pc.Spec.CredentialsRef.Key
	if secretKey == "" {
		secretKey = "kubeconfig"
	}

	kubeconfig, ok := secret.Data[secretKey]
	if !ok {
		return nil, fmt.Errorf("credentials secret %s does not contain key %s", key, secretKey)
	}

	return harvester.NewClient(kubeconfig, pc.Spec.Harvester)
}

func (r *MachineRequestReconciler) updatePhase(ctx context.Context, mr *butlerv1alpha1.MachineRequest, phase butlerv1alpha1.MachinePhase) (ctrl.Result, error) {
	mr.Status.Phase = phase
	now := metav1.Now()
	mr.Status.LastUpdated = &now
	if err := r.Status().Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *MachineRequestReconciler) updateStatusError(ctx context.Context, mr *butlerv1alpha1.MachineRequest, reason, message string) (ctrl.Result, error) {
	mr.SetFailure(reason, message)
	meta.SetStatusCondition(&mr.Status.Conditions, metav1.Condition{
		Type:               butlerv1alpha1.ConditionTypeReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: mr.Generation,
	})
	if err := r.Status().Update(ctx, mr); err != nil {
		return ctrl.Result{}, err
	}
	r.Recorder.Event(mr, corev1.EventTypeWarning, reason, message)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MachineRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&butlerv1alpha1.MachineRequest{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("machinerequest").
		Complete(r)
}
