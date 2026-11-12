// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package scenario

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Profile chooses provider responses for one test case.
type Profile string

// Settings labels bind one case to its original InfraEnv, HostedCluster and CAPI Cluster UIDs.
const (
	settingsLabel         = "simulator.osac-project.io/settings"
	infraEnvUIDLabel      = "simulator.osac-project.io/infraenv-uid"
	hostedClusterUIDLabel = "simulator.osac-project.io/hostedcluster-uid"
	clusterUIDLabel       = "simulator.osac-project.io/cluster-uid"
	configMapKey          = "profile"
	discoveryURLKey       = "discoveryIgnitionURL"
)

const (
	Normal              Profile = "normal"
	InstallationFailed  Profile = "installation-failed"
	InstallationStalled Profile = "installation-stalled"
	DetachStalled       Profile = "detach-stalled"
	ControlPlaneStalled Profile = "control-plane-stalled"
)

// validateProfile returns an error for any name outside the five supported profiles.
// Callers check the name before storing or applying response settings.
func validateProfile(profile Profile) error {
	switch profile {
	case Normal, InstallationFailed, InstallationStalled, DetachStalled, ControlPlaneStalled:
		return nil
	}

	return fmt.Errorf("unknown provider profile %q", profile)
}

// Scenario owns one case's desired responses; provider resources hold all progress.
type Scenario struct {
	Kube      kubernetes.Interface
	Namespace string
	Name      string
	uid       types.UID
}

// SetProfile updates only the ConfigMap holding the live response selection.
// Reconcilers apply the selection on their next pass; product assertions await progress.
func (environment *Scenario) SetProfile(ctx context.Context, profile Profile) error {
	if err := validateProfile(profile); err != nil {
		return err
	}

	api := environment.Kube.CoreV1().ConfigMaps(environment.Namespace)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		settings, err := api.Get(ctx, environment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if settings.UID != environment.uid {
			return fmt.Errorf("case settings were replaced")
		}

		if settings.Data[configMapKey] == string(profile) {
			return nil
		}

		if settings.Data == nil {
			settings.Data = map[string]string{}
		}

		settings.Data[configMapKey] = string(profile)
		_, err = api.Update(ctx, settings, metav1.UpdateOptions{})

		return err
	}); err != nil {
		return err
	}

	slog.InfoContext(ctx, "simulator profile configured",
		"namespace", environment.Namespace,
		"settings", environment.Name,
		"profile", profile,
	)

	return nil
}

// Create stores the case profile and discovery URL, retaining the ConfigMap UID.
// The caller must create these settings before submitting the cluster request.
func (environment *Scenario) Create(ctx context.Context, profile Profile, discoveryURL string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}

	endpoint, err := url.Parse(discoveryURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return fmt.Errorf("discovery ignition URL must be an absolute HTTP URL")
	}

	data := map[string]string{configMapKey: string(profile), discoveryURLKey: discoveryURL}

	created, err := environment.Kube.CoreV1().
		ConfigMaps(environment.Namespace).
		Create(
			ctx,
			&corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:   environment.Name,
					Labels: map[string]string{settingsLabel: "true"},
				},
				Data: data,
			},
			metav1.CreateOptions{},
		)
	if err == nil {
		environment.uid = created.UID
	}

	return err
}

// BindInfraEnv binds this case to its original InfraEnv UID.
func (environment *Scenario) BindInfraEnv(ctx context.Context, uid types.UID) error {
	return environment.bind(ctx, infraEnvUIDLabel, uid)
}

// BindHostedCluster binds this case to its original HostedCluster UID.
func (environment *Scenario) BindHostedCluster(ctx context.Context, uid types.UID) error {
	return environment.bind(ctx, hostedClusterUIDLabel, uid)
}

// BindCluster binds this case to its original CAPI Cluster UID.
// A surviving Cluster cannot supply responses to a second case.
func (environment *Scenario) BindCluster(ctx context.Context, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("native root UID is required")
	}

	settings, err := environment.Kube.CoreV1().ConfigMaps(environment.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{
			settingsLabel: "true", clusterUIDLabel: string(uid),
		}).String(),
	})
	if err != nil {
		return err
	}

	for _, current := range settings.Items {
		if current.Name != environment.Name || current.UID != environment.uid {
			return fmt.Errorf("cluster %s is bound to another case", uid)
		}
	}

	return environment.bind(ctx, clusterUIDLabel, uid)
}

// bind records a native root UID and rejects replacement bindings.
func (environment *Scenario) bind(ctx context.Context, key string, uid types.UID) error {
	if uid == "" {
		return fmt.Errorf("native root UID is required")
	}

	api := environment.Kube.CoreV1().ConfigMaps(environment.Namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		settings, err := api.Get(ctx, environment.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if settings.UID != environment.uid {
			return fmt.Errorf("case settings were replaced")
		}

		if current := settings.Labels[key]; current != "" {
			if current != string(uid) {
				return fmt.Errorf("case root incarnation changed")
			}

			return nil
		}

		settings.Labels[key] = string(uid)
		_, err = api.Update(ctx, settings, metav1.UpdateOptions{})

		return err
	})
}

// Delete removes this case's ConfigMap using its recorded UID.
// The caller must first verify resource cleanup and terminal automation.
func (environment *Scenario) Delete(ctx context.Context) error {
	return environment.Kube.CoreV1().
		ConfigMaps(environment.Namespace).
		Delete(ctx, environment.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &environment.uid}})
}
