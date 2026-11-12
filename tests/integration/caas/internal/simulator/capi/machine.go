// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

// MachineReconciler adopts its concrete AgentMachine, reports provider readiness
// and waits for CAP-Agent to release a host before deleting its provider.
type MachineReconciler struct {
	Client    client.Client
	APIReader client.Reader
}

// Reconcile adopts the MachineSet's clone after checking its owner, then copies
// infrastructure readiness. Guest Node readiness and drain are outside this model.
func (r *MachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	machine := &capiv1.Machine{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if _, paused := machine.Annotations[capiv1.PausedAnnotation]; paused {
		return ctrl.Result{}, nil
	}

	cluster := &capiv1.Cluster{}
	if err := r.APIReader.Get(
		ctx,
		client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.ClusterName},
		cluster,
	); err != nil &&
		!apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if cluster.Spec.Paused {
		return ctrl.Result{}, nil
	}

	if machine.DeletionTimestamp != nil {
		return r.delete(ctx, machine)
	}

	ref := machine.Spec.InfrastructureRef
	if ref.APIVersion != agentv1.GroupVersion.String() || ref.Kind != "AgentMachine" {
		return ctrl.Result{}, fmt.Errorf("machine requires a concrete AgentMachine reference")
	}

	infra := &agentv1.AgentMachine{}

	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}, infra)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	if err := validateInfrastructureOwner(machine, infra); err != nil {
		return ctrl.Result{}, err
	}

	if err := kube.AddFinalizer(ctx, r.Client, machine, machineFinalizer); err != nil {
		return ctrl.Result{}, err
	}

	if !controllerutil.HasControllerReference(infra) {
		before := infra.DeepCopy()
		if err := controllerutil.SetControllerReference(machine, infra, r.Client.Scheme()); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.Client.Patch(ctx, infra,
			client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, r.reportMachine(ctx, machine, infra)
}

// reportMachine publishes bootstrap, provider identity and current conditions.
// Infrastructure provisioning is sticky independently of its mirrored condition.
func (r *MachineReconciler) reportMachine(
	ctx context.Context,
	machine *capiv1.Machine,
	infra *agentv1.AgentMachine,
) error {
	next := machine.DeepCopy()

	if infra.Status.Ready || machine.Status.InfrastructureReady {
		if infra.Spec.ProviderID == nil || *infra.Spec.ProviderID == "" {
			return fmt.Errorf("ready AgentMachine has no providerID")
		}

		next.Spec.ProviderID = new(*infra.Spec.ProviderID)
		next.Status.InfrastructureReady = true
	}

	if machine.Spec.Bootstrap.DataSecretName != nil {
		next.Status.BootstrapReady = true
		conditions.MarkTrue(next, capiv1.BootstrapReadyCondition)
	} else {
		conditions.MarkFalse(
			next,
			capiv1.BootstrapReadyCondition,
			"WaitingForBootstrapData",
			capiv1.ConditionSeverityInfo,
			"Waiting for bootstrap data",
		)
	}

	conditions.SetMirror(
		next,
		capiv1.InfrastructureReadyCondition,
		infra,
		conditions.WithFallbackValue(
			infra.Status.Ready,
			capiv1.WaitingForInfrastructureFallbackReason,
			capiv1.ConditionSeverityInfo,
			"",
		),
	)
	conditions.SetSummary(
		next,
		conditions.WithConditions(capiv1.InfrastructureReadyCondition, capiv1.BootstrapReadyCondition),
	)

	if !equality.Semantic.DeepEqual(machine.Spec, next.Spec) {
		// The spec response contains stored status; retain these new observations separately.
		spec := next.DeepCopy()
		if err := r.Client.Patch(
			ctx,
			spec,
			client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{}),
		); err != nil {
			return err
		}

		machine.Spec, machine.ResourceVersion = spec.Spec, spec.ResourceVersion
		next.ResourceVersion = spec.ResourceVersion
	}

	if equality.Semantic.DeepEqual(machine.Status, next.Status) {
		return nil
	}

	return r.Client.Status().Patch(ctx, next,
		client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{}))
}

// validateInfrastructureOwner accepts this Machine's provider or its MachineSet's
// pending clone, keeping other owners' resources unchanged during adoption and deletion.
func validateInfrastructureOwner(machine *capiv1.Machine, infrastructure *agentv1.AgentMachine) error {
	if controllerutil.HasControllerReference(infrastructure) {
		if kube.MatchesController(infrastructure, machine, capiv1.GroupVersion.WithKind("Machine")) {
			return nil
		}

		return fmt.Errorf("AgentMachine %s belongs to another controller", infrastructure.Name)
	}

	set := metav1.GetControllerOf(machine)
	if set != nil && set.Kind == "MachineSet" {
		for _, owner := range infrastructure.OwnerReferences {
			if owner.APIVersion == set.APIVersion && owner.Kind == set.Kind && owner.Name == set.Name && owner.UID == set.UID {
				return nil
			}
		}
	}

	return fmt.Errorf("AgentMachine %s belongs to another MachineSet incarnation", infrastructure.Name)
}

// delete waits for CAP-Agent's hook, then deletes the private provider and waits
// for absence before removing the Machine finalizer. Shared templates remain.
func (r *MachineReconciler) delete(ctx context.Context, machine *capiv1.Machine) (ctrl.Result, error) {
	if _, held := machine.Annotations[kube.MachineHook]; held {
		next := machine.DeepCopy()
		conditions.Set(next, &capiv1.Condition{
			Type:   capiv1.PreTerminateDeleteHookSucceededCondition,
			Status: corev1.ConditionFalse, Reason: "WaitingExternalHook",
		})

		if equality.Semantic.DeepEqual(machine.Status, next.Status) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}

		return ctrl.Result{RequeueAfter: time.Second}, r.Client.Status().Patch(ctx, next,
			client.MergeFromWithOptions(machine, client.MergeFromWithOptimisticLock{}))
	}

	if machine.Spec.InfrastructureRef.Kind != "AgentMachine" {
		return ctrl.Result{}, kube.RemoveFinalizer(ctx, r.Client, machine, machineFinalizer)
	}

	infra := &agentv1.AgentMachine{}

	err := r.APIReader.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace, Name: machine.Spec.InfrastructureRef.Name,
	}, infra)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, kube.RemoveFinalizer(ctx, r.Client, machine, machineFinalizer)
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	if err := validateInfrastructureOwner(machine, infra); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: time.Second}, kube.Delete(ctx, r.Client, infra)
}
