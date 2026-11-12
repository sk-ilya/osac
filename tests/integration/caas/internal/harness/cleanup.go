// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"fmt"
	"log/slog"

	installv1 "github.com/openshift/assisted-service/api/hiveextension/v1beta1"
	assistedv1 "github.com/openshift/assisted-service/api/v1beta1"
	agentv1 "github.com/openshift/cluster-api-provider-agent/api/v1beta1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	hypershiftv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	capiv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CheckNativeResourcesGone checks dedicated-installation inputs and provider outputs.
func CheckNativeResourcesGone(ctx context.Context, environment *Environment) error {
	if err := checkNativeInputsGone(ctx, environment); err != nil {
		return err
	}

	if err := checkControllerResourcesGone(ctx, environment); err != nil {
		return err
	}

	return checkSimulatorOutputsGone(ctx, environment)
}

// checkNativeInputsGone checks cluster-wide roots and hub-scoped discovery inputs.
func checkNativeInputsGone(ctx context.Context, environment *Environment) error {
	var hostedClusters hypershiftv1.HostedClusterList

	if err := environment.Client.List(ctx, &hostedClusters); err != nil {
		return err
	}

	if len(hostedClusters.Items) != 0 {
		slog.DebugContext(ctx, "native inputs remain", "resource", "hostedclusters", "count", len(hostedClusters.Items))
		return fmt.Errorf("%d hostedclusters remain", len(hostedClusters.Items))
	}

	var pools hypershiftv1.NodePoolList

	if err := environment.Client.List(ctx, &pools); err != nil {
		return err
	}

	if len(pools.Items) != 0 {
		slog.DebugContext(ctx, "native inputs remain", "resource", "nodepools", "count", len(pools.Items))
		return fmt.Errorf("%d nodepools remain", len(pools.Items))
	}

	var infraEnvs assistedv1.InfraEnvList

	if err := environment.Client.List(ctx, &infraEnvs, client.InNamespace(environment.Namespace)); err != nil {
		return err
	}

	if len(infraEnvs.Items) != 0 {
		slog.DebugContext(ctx, "native inputs remain", "resource", "infraenvs", "count", len(infraEnvs.Items))
		return fmt.Errorf("%d infraenvs remain", len(infraEnvs.Items))
	}

	var agents assistedv1.AgentList

	if err := environment.Client.List(ctx, &agents, client.InNamespace(environment.Namespace)); err != nil {
		return err
	}

	if len(agents.Items) != 0 {
		slog.DebugContext(ctx, "native inputs remain", "resource", "agents", "count", len(agents.Items))
		return fmt.Errorf("%d agents remain", len(agents.Items))
	}

	return nil
}

// checkControllerResourcesGone checks provider records across all working namespaces.
func checkControllerResourcesGone(ctx context.Context, environment *Environment) error {
	var controlPlanes hypershiftv1.HostedControlPlaneList

	if err := environment.Client.List(ctx, &controlPlanes); err != nil {
		return err
	}

	if len(controlPlanes.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"hostedcontrolplanes",
			"count",
			len(controlPlanes.Items),
		)

		return fmt.Errorf("%d native hostedcontrolplanes remain", len(controlPlanes.Items))
	}

	var clusters capiv1.ClusterList

	if err := environment.Client.List(ctx, &clusters); err != nil {
		return err
	}

	if len(clusters.Items) != 0 {
		slog.DebugContext(ctx, "native controller resources remain", "resource", "clusters", "count", len(clusters.Items))
		return fmt.Errorf("%d native clusters remain", len(clusters.Items))
	}

	var sets capiv1.MachineSetList

	if err := environment.Client.List(ctx, &sets); err != nil {
		return err
	}

	if len(sets.Items) != 0 {
		slog.DebugContext(ctx, "native controller resources remain", "resource", "machinesets", "count", len(sets.Items))
		return fmt.Errorf("%d native machinesets remain", len(sets.Items))
	}

	var machines capiv1.MachineList

	if err := environment.Client.List(ctx, &machines); err != nil {
		return err
	}

	if len(machines.Items) != 0 {
		slog.DebugContext(ctx, "native controller resources remain", "resource", "machines", "count", len(machines.Items))
		return fmt.Errorf("%d native machines remain", len(machines.Items))
	}

	var agentClusters agentv1.AgentClusterList

	if err := environment.Client.List(ctx, &agentClusters); err != nil {
		return err
	}

	if len(agentClusters.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"agentclusters",
			"count",
			len(agentClusters.Items),
		)

		return fmt.Errorf("%d native agentclusters remain", len(agentClusters.Items))
	}

	var agentMachines agentv1.AgentMachineList

	if err := environment.Client.List(ctx, &agentMachines); err != nil {
		return err
	}

	if len(agentMachines.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"agentmachines",
			"count",
			len(agentMachines.Items),
		)

		return fmt.Errorf("%d native agentmachines remain", len(agentMachines.Items))
	}

	var templates agentv1.AgentMachineTemplateList

	if err := environment.Client.List(ctx, &templates); err != nil {
		return err
	}

	if len(templates.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"agentmachinetemplates",
			"count",
			len(templates.Items),
		)

		return fmt.Errorf("%d native agentmachinetemplates remain", len(templates.Items))
	}

	var deployments hivev1.ClusterDeploymentList

	if err := environment.Client.List(ctx, &deployments); err != nil {
		return err
	}

	if len(deployments.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"clusterdeployments",
			"count",
			len(deployments.Items),
		)

		return fmt.Errorf("%d native clusterdeployments remain", len(deployments.Items))
	}

	var installations installv1.AgentClusterInstallList

	if err := environment.Client.List(ctx, &installations); err != nil {
		return err
	}

	if len(installations.Items) != 0 {
		slog.DebugContext(
			ctx,
			"native controller resources remain",
			"resource",
			"agentclusterinstalls",
			"count",
			len(installations.Items),
		)

		return fmt.Errorf("%d native agentclusterinstalls remain", len(installations.Items))
	}

	return nil
}

// checkSimulatorOutputsGone checks labeled outputs, including cross-namespace Agents.
func checkSimulatorOutputsGone(ctx context.Context, environment *Environment) error {
	managed := client.MatchingLabels{"app.kubernetes.io/managed-by": "osac-provider-simulator"}

	var agents assistedv1.AgentList

	if err := environment.Client.List(ctx, &agents, managed); err != nil {
		return err
	}

	if len(agents.Items) != 0 {
		slog.DebugContext(ctx, "simulator outputs remain", "resource", "agents", "count", len(agents.Items))
		return fmt.Errorf("%d simulator-owned agents remain", len(agents.Items))
	}

	var namespaces corev1.NamespaceList

	if err := environment.Client.List(ctx, &namespaces, managed); err != nil {
		return err
	}

	if len(namespaces.Items) != 0 {
		slog.DebugContext(ctx, "simulator outputs remain", "resource", "namespaces", "count", len(namespaces.Items))
		return fmt.Errorf("%d simulator-owned namespaces remain", len(namespaces.Items))
	}

	var services corev1.ServiceList

	if err := environment.Client.List(ctx, &services, managed); err != nil {
		return err
	}

	if len(services.Items) != 0 {
		slog.DebugContext(ctx, "simulator outputs remain", "resource", "services", "count", len(services.Items))
		return fmt.Errorf("%d simulator-owned services remain", len(services.Items))
	}

	var secrets corev1.SecretList

	if err := environment.Client.List(ctx, &secrets, managed); err != nil {
		return err
	}

	if len(secrets.Items) != 0 {
		slog.DebugContext(ctx, "simulator outputs remain", "resource", "secrets", "count", len(secrets.Items))
		return fmt.Errorf("%d simulator-owned secrets remain", len(secrets.Items))
	}

	return nil
}
