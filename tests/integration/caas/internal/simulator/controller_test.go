// Copyright (c) 2026 Red Hat Inc.
// SPDX-License-Identifier: Apache-2.0

package simulator

import (
	"errors"
	"net/http"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var _ = Describe("Simulator startup configuration", func() {
	var (
		config    Config
		transport *rejectingTransport
	)

	BeforeEach(func() {
		transport = &rejectingTransport{}
		config = Config{
			RESTConfig:        &rest.Config{Host: "https://simulator.invalid", Transport: transport},
			AgentNamespaces:   []string{"agents"},
			SettingsNamespace: "release",
			Metal3Namespace:   "inventory",
		}
	})

	It("accepts complete configuration without opening clients", func() {
		Expect(config.validate()).To(Succeed())
	})

	DescribeTable(
		"rejects incomplete configuration before API contact",
		func(ctx SpecContext, invalidate func(*Config), wantError string) {
			invalidate(&config)
			Expect(Run(ctx, config)).To(MatchError(wantError))
			Expect(transport.requests.Load()).To(BeZero(), "invalid configuration must not contact Kubernetes")
		},
		Entry(
			"missing connection",
			func(c *Config) { c.RESTConfig = nil },
			"cluster connection, settings namespace and Agent namespaces are required",
		),
		Entry(
			"missing settings namespace",
			func(c *Config) { c.SettingsNamespace = "" },
			"cluster connection, settings namespace and Agent namespaces are required",
		),
		Entry(
			"missing Agent namespaces",
			func(c *Config) { c.AgentNamespaces = nil },
			"cluster connection, settings namespace and Agent namespaces are required",
		),
		Entry("empty Agent namespace", func(c *Config) { c.AgentNamespaces = []string{""} }, "empty Agent namespace"),
		Entry(
			"missing inventory namespace",
			func(c *Config) { c.Metal3Namespace = "" },
			"Metal3 inventory namespace is required",
		),
	)
})

var _ = Describe("Manager health checks", func() {
	It("keeps process health independent of cache readiness through startup and shutdown", func() {
		mgr := &healthCheckManager{}

		var ready atomic.Bool

		Expect(addHealthChecks(mgr, &ready)).To(Succeed())
		Expect(mgr.health(nil)).To(Succeed())
		Expect(mgr.ready(nil)).To(MatchError("starting simulator caches"))

		ready.Store(true)
		Expect(mgr.ready(nil)).To(Succeed())

		ready.Store(false)
		Expect(mgr.ready(nil)).To(MatchError("starting simulator caches"))
		Expect(mgr.health(nil)).To(Succeed())
	})

	DescribeTable("propagates manager registration errors", func(healthFails bool) {
		want := errors.New("registration failed")
		mgr := &healthCheckManager{}
		if healthFails {
			mgr.healthError = want
		} else {
			mgr.readyError = want
		}

		Expect(addHealthChecks(mgr, &atomic.Bool{})).To(MatchError(want))
	},
		Entry("health check", true),
		Entry("readiness check", false),
	)
})

// healthCheckManager records checks without opening a listener or Kubernetes client.
type healthCheckManager struct {
	manager.Manager
	health, ready           healthz.Checker
	healthError, readyError error
}

// AddHealthzCheck records the process liveness check.
func (m *healthCheckManager) AddHealthzCheck(_ string, check healthz.Checker) error {
	m.health = check
	return m.healthError
}

// AddReadyzCheck records the cache synchronization check.
func (m *healthCheckManager) AddReadyzCheck(_ string, check healthz.Checker) error {
	m.ready = check
	return m.readyError
}

type rejectingTransport struct {
	requests atomic.Int32
}

// RoundTrip records and rejects API requests locally so validation tests cannot reach Kubernetes.
func (t *rejectingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.requests.Add(1)
	return nil, errors.New("unexpected API request")
}
