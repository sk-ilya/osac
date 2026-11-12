// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

// Package cpo simulates control-plane operator responses on HostedControlPlanes.
// Guest processes are represented by native status and non-functional access data.
package cpo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/osac-project/osac/tests/integration/caas/internal/simulator/kube"
)

const controlPlaneFinalizer = "hypershift.openshift.io/finalizer"

// HostedControlPlaneReconciler creates the HCP's API and ignition Services, CA and
// kubeconfig Secrets, then publishes references and the selected availability response.
type HostedControlPlaneReconciler struct {
	Client               client.Client
	APIReader            client.Reader
	Scope                kube.Scope
	DefaultIngressDomain string
	Availability         func(context.Context, *hypershiftv1.HostedControlPlane) (bool, error)
}

// Reconcile serves None-platform HCPs created for Agent hosted clusters in scope.
// Endpoint and kubeconfig inputs remain available during an availability hold so
// worker installation can progress independently of control-plane readiness.
func (r *HostedControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	controlPlane := &hypershiftv1.HostedControlPlane{}
	if err := r.APIReader.Get(ctx, req.NamespacedName, controlPlane); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if controlPlane.Spec.Platform.Type != hypershiftv1.NonePlatform || controlPlane.Spec.Platform.Agent == nil ||
		!r.Scope.Contains(controlPlane.Spec.Platform.Agent.AgentNamespace) {
		return ctrl.Result{}, nil
	}

	if controlPlane.DeletionTimestamp != nil {
		// None-platform cleanup finishes without cloud-provider work.
		return ctrl.Result{}, kube.RemoveFinalizer(ctx, r.Client, controlPlane, controlPlaneFinalizer)
	}

	if err := kube.AddFinalizer(ctx, r.Client, controlPlane, controlPlaneFinalizer); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.observeReconciliationActive(ctx, controlPlane); err != nil {
		return ctrl.Result{}, err
	}

	if paused, duration := reconciliationPause(ctx, controlPlane.Spec.PausedUntil); paused {
		return ctrl.Result{RequeueAfter: duration}, nil
	}

	service, err := r.ensureAPIService(ctx, controlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureIgnition(ctx, controlPlane); err != nil {
		return ctrl.Result{}, err
	}

	secret, err := r.ensureKubeconfig(ctx, controlPlane, service)
	if err != nil {
		return ctrl.Result{}, err
	}

	kubeconfigName := ""
	if secret != nil {
		kubeconfigName = secret.Name
	}

	available, observationError := r.Availability(ctx, controlPlane)

	next := controlPlaneStatus(controlPlane, service, kubeconfigName, available && observationError == nil)
	if observationError != nil {
		meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
			Type: "Degraded", Status: metav1.ConditionTrue, Reason: "ReconciliationError",
			Message: observationError.Error(), ObservedGeneration: controlPlane.Generation,
		})
	}

	if !equality.Semantic.DeepEqual(controlPlane.Status, next.Status) {
		if err := r.Client.Status().Patch(ctx, next,
			client.MergeFromWithOptions(controlPlane, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, errors.Join(observationError, err)
		}
	}

	// Poll the live response even after readiness; unchanged status writes are skipped.
	return ctrl.Result{RequeueAfter: time.Second}, observationError
}

// observeReconciliationActive reports pause diagnostics independently of output readiness.
func (r *HostedControlPlaneReconciler) observeReconciliationActive(
	ctx context.Context, cp *hypershiftv1.HostedControlPlane,
) error {
	before := cp.DeepCopy()
	meta.SetStatusCondition(&cp.Status.Conditions, reconciliationActiveCondition(cp.Spec.PausedUntil, cp.Generation))

	if equality.Semantic.DeepEqual(before.Status, cp.Status) {
		return nil
	}

	return r.Client.Status().Patch(ctx, cp, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// reconciliationPause parses native Boolean and timestamp values, logging malformed input as unpaused.
func reconciliationPause(ctx context.Context, value *string) (bool, time.Duration) {
	if value == nil {
		return false, 0
	}

	if paused, err := strconv.ParseBool(*value); err == nil {
		return paused, 0
	}

	until, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "invalid pausedUntil; considering control plane unpaused")

		return false, 0
	}

	duration := time.Until(until)

	return duration > 0, duration
}

// reconciliationActiveCondition mirrors pinned HyperShift's pausedUntil status.
func reconciliationActiveCondition(value *string, generation int64) metav1.Condition {
	condition := metav1.Condition{
		Type: string(hypershiftv1.ReconciliationActive), Status: metav1.ConditionTrue,
		Reason: hypershiftv1.AsExpectedReason, Message: "Reconciliation active on resource",
		ObservedGeneration: generation,
	}
	if value == nil {
		return condition
	}

	paused, err := strconv.ParseBool(*value)
	if err == nil {
		if paused {
			condition.Status, condition.Reason = metav1.ConditionFalse, hypershiftv1.ReconciliationPausedConditionReason
			condition.Message = "Reconciliation paused until field removed"
		}

		return condition
	}

	until, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		condition.Reason = hypershiftv1.ReconciliationInvalidPausedUntilConditionReason
		condition.Message = "Invalid value provided for PausedUntil field"
		return condition
	}

	if time.Now().Before(until) {
		condition.Status, condition.Reason = metav1.ConditionFalse, hypershiftv1.ReconciliationPausedConditionReason
		condition.Message = fmt.Sprintf("Reconciliation paused until: %s", *value)
	}

	return condition
}

// controlPlaneStatus returns endpoint, access, readiness and controller health status.
// It keeps worker connection details available while control-plane availability is held.
func controlPlaneStatus(
	controlPlane *hypershiftv1.HostedControlPlane,
	service *corev1.Service,
	kubeconfigName string,
	available bool,
) *hypershiftv1.HostedControlPlane {
	address, port := apiEndpoint(controlPlane, service)

	ready := address != "" && available
	status, reason := metav1.ConditionFalse, "ControlPlaneStarting"
	if ready {
		status, reason = metav1.ConditionTrue, hypershiftv1.AsExpectedReason
	}

	next := controlPlane.DeepCopy()
	next.Status.Ready = ready

	// The simulated backend is healthy independently of control-plane availability.
	meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
		Type: "Degraded", Status: metav1.ConditionFalse, Reason: hypershiftv1.AsExpectedReason,
		ObservedGeneration: controlPlane.Generation,
	})

	// Management-side inputs stand in for guest initialization; availability is observed separately.
	if kubeconfigName != "" {
		next.Status.Initialized = true
		next.Status.KubeConfig = &hypershiftv1.KubeconfigSecretRef{Name: kubeconfigName, Key: "kubeconfig"}
	}

	if address != "" {
		next.Status.ControlPlaneEndpoint = hypershiftv1.APIEndpoint{Host: address, Port: port}
	}

	for _, kind := range []string{"Available", "KubeAPIServerAvailable"} {
		meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
			Type: kind, Status: status, Reason: reason,
			Message: "Simulated provider observation", ObservedGeneration: controlPlane.Generation,
		})
	}

	infrastructureStatus := metav1.ConditionFalse
	infrastructureReason := hypershiftv1.WaitingOnInfrastructureReadyReason
	message := "Cluster infrastructure is still provisioning"

	if address != "" {
		infrastructureStatus = metav1.ConditionTrue
		infrastructureReason, message = hypershiftv1.AsExpectedReason, hypershiftv1.AllIsWellMessage
	}

	meta.SetStatusCondition(&next.Status.Conditions, metav1.Condition{
		Type: "InfrastructureReady", Status: infrastructureStatus, Reason: infrastructureReason,
		Message: message, ObservedGeneration: controlPlane.Generation,
	})

	return next
}

// apiStrategy finds the required API-server publication contract.
func apiStrategy(controlPlane *hypershiftv1.HostedControlPlane) *hypershiftv1.ServicePublishingStrategy {
	for i := range controlPlane.Spec.Services {
		if controlPlane.Spec.Services[i].Service == hypershiftv1.APIServer {
			return &controlPlane.Spec.Services[i].ServicePublishingStrategy
		}
	}

	return nil
}

// ensureAPIService converges CPO-owned Service fields while preserving allocated
// addresses, node ports and fields managed by the API server or other controllers.
func (r *HostedControlPlaneReconciler) ensureAPIService(
	ctx context.Context,
	controlPlane *hypershiftv1.HostedControlPlane,
) (*corev1.Service, error) {
	key := client.ObjectKey{Namespace: controlPlane.Namespace, Name: "kube-apiserver"}
	service := &corev1.Service{}

	err := r.APIReader.Get(ctx, key, service)
	create := apierrors.IsNotFound(err)
	if err != nil && !create {
		return nil, err
	}

	if create {
		service.Namespace, service.Name = key.Namespace, key.Name
		if err := controllerutil.SetControllerReference(controlPlane, service, r.Client.Scheme()); err != nil {
			return nil, err
		}
	} else if !kube.MatchesController(service, controlPlane, hypershiftv1.GroupVersion.WithKind("HostedControlPlane")) {
		return nil, fmt.Errorf("service %s/%s belongs to another control plane", key.Namespace, key.Name)
	}

	if service.DeletionTimestamp != nil {
		return service, nil
	}

	before := service.DeepCopy()
	if err := apiServiceSpec(service, controlPlane); err != nil {
		return nil, err
	}

	if create {
		return service, r.Client.Create(ctx, service)
	}

	if equality.Semantic.DeepEqual(before.Spec, service.Spec) &&
		equality.Semantic.DeepEqual(before.Labels, service.Labels) &&
		equality.Semantic.DeepEqual(before.Annotations, service.Annotations) {
		return service, nil
	}

	return service, r.Client.Patch(ctx, service, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

// apiEndpoint applies configured hostname precedence to the observed Service address.
func apiEndpoint(controlPlane *hypershiftv1.HostedControlPlane, service *corev1.Service) (string, int32) {
	port := int32(6443)
	if len(service.Spec.Ports) > 0 {
		port = service.Spec.Ports[0].Port
	}

	strategy := apiStrategy(controlPlane)
	if strategy == nil {
		return "", port
	}

	switch strategy.Type {
	case hypershiftv1.NodePort:
		if strategy.NodePort == nil || len(service.Spec.Ports) == 0 || service.Spec.Ports[0].NodePort == 0 {
			return "", port
		}

		return strategy.NodePort.Address, service.Spec.Ports[0].NodePort
	case hypershiftv1.LoadBalancer:
		if len(service.Status.LoadBalancer.Ingress) == 0 {
			return "", port
		}

		if strategy.LoadBalancer != nil && strategy.LoadBalancer.Hostname != "" {
			return strategy.LoadBalancer.Hostname, port
		}

		if service.Status.LoadBalancer.Ingress[0].Hostname != "" {
			return service.Status.LoadBalancer.Ingress[0].Hostname, port
		}

		return service.Status.LoadBalancer.Ingress[0].IP, port
	default:
		return "", port
	}
}

// apiServiceSpec writes only CPO's native publishing fields on a validated Service.
func apiServiceSpec(service *corev1.Service, controlPlane *hypershiftv1.HostedControlPlane) error {
	strategy := apiStrategy(controlPlane)
	if strategy == nil {
		return fmt.Errorf("API server publishing strategy is required")
	}

	metav1.SetMetaDataLabel(&service.ObjectMeta, "app", "kube-apiserver")

	if service.Spec.Selector == nil {
		service.Spec.Selector = map[string]string{"app": "kube-apiserver"}
	}

	service.Spec.IPFamilyPolicy = new(corev1.IPFamilyPolicyPreferDualStack)
	if len(service.Spec.Ports) == 0 {
		service.Spec.Ports = []corev1.ServicePort{{}}
	}

	port := &service.Spec.Ports[0]

	port.Port, port.Protocol, port.TargetPort = 6443, corev1.ProtocolTCP, intstr.FromString("client")
	service.Spec.LoadBalancerSourceRanges = nil
	if networking := controlPlane.Spec.Networking.APIServer; networking != nil {
		if networking.Port != nil {
			port.Port = *networking.Port
		}

		for _, cidr := range networking.AllowedCIDRBlocks {
			service.Spec.LoadBalancerSourceRanges = append(service.Spec.LoadBalancerSourceRanges, string(cidr))
		}
	}

	switch strategy.Type {
	case hypershiftv1.LoadBalancer:
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		if strategy.LoadBalancer != nil && strategy.LoadBalancer.Hostname != "" {
			metav1.SetMetaDataAnnotation(
				&service.ObjectMeta,
				hypershiftv1.ExternalDNSHostnameAnnotation,
				strategy.LoadBalancer.Hostname,
			)
		}
	case hypershiftv1.NodePort:
		service.Spec.Type = corev1.ServiceTypeNodePort

		if port.NodePort == 0 && strategy.NodePort != nil {
			port.NodePort = strategy.NodePort.Port
		}
	default:
		return fmt.Errorf("unsupported API server publishing strategy %q", strategy.Type)
	}

	return nil
}

// SetupWithManager watches HostedControlPlanes and their owned Services, Secrets and Routes.
// Child updates let published addresses and access data wake their control plane.
func (r *HostedControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("simulated-cpo-hostedcontrolplane").
		For(&hypershiftv1.HostedControlPlane{}).
		Owns(&corev1.Service{}).Owns(&corev1.Secret{}).Owns(&routev1.Route{}).
		WithOptions(kube.ControllerOptions()).Complete(r)
}
