// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package serviceentry

import (
	"fmt"
	"testing"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/sets"
	"istio.io/istio/platform/discovery/peering"
)

func createPeered(t test.Failer, store model.ConfigStore, name string, realNamespace string) {
	t.Helper()
	_, err := store.Create(config.Config{
		Meta: config.Meta{
			Name:      name,
			Namespace: peering.PeeringNamespace,
			Labels: map[string]string{
				peering.ParentServiceNamespaceLabel: realNamespace,
				peering.ParentServiceLabel:          "some-service",
			},
			GroupVersionKind: gvk.WorkloadEntry,
		},
		Spec: &networking.WorkloadEntry{},
	})
	assert.NoError(t, err)
}

// nolint: unparam
func createStandard(t test.Failer, store model.ConfigStore, name string, realNamespace string) {
	t.Helper()
	_, err := store.Create(config.Config{
		Meta: config.Meta{
			Name:             name,
			Namespace:        realNamespace,
			GroupVersionKind: gvk.WorkloadEntry,
		},
		Spec: &networking.WorkloadEntry{},
	})
	assert.NoError(t, err)
}

func TestPeeringNamespaceWrapper(t *testing.T) {
	assertHasObjects := func(t test.Failer, got []config.Config, want ...string) {
		t.Helper()
		gotNames := sets.New[string](slices.Map(got, func(e config.Config) string {
			return config.NamespacedName(e).String()
		})...)
		wantNames := sets.New(want...)
		assert.Equal(t, gotNames, wantNames)
	}
	assertHasObjectsOrdered := func(t test.Failer, got []config.Config, want ...string) {
		t.Helper()
		gotNames := slices.Map(got, func(e config.Config) string {
			return config.NamespacedName(e).String()
		})
		assert.Equal(t, gotNames, want)
	}
	t.Run("list namespaced", func(t *testing.T) {
		wrapped := setupBase(t)
		assertHasObjects(t, wrapped.List(gvk.WorkloadEntry, "default"),
			"default/real-obj",
			"default/real-obj2",
			"default/peer-obj1",
		)
	})
	t.Run("list all", func(t *testing.T) {
		wrapped := setupBase(t)
		assertHasObjects(t, wrapped.List(gvk.WorkloadEntry, ""),
			"default/real-obj",
			"default/real-obj2",
			"default/peer-obj1",
			"not-default/peer-obj2",
		)
	})
	t.Run("conflicts", func(t *testing.T) {
		wrapped := setupEmpty(t)
		createStandard(t, wrapped, "obj", "default")
		createPeered(t, wrapped, "obj", "default")
		// Currently we do not dedupe objects of the same name
		// TODO: fix this
		assertHasObjectsOrdered(t, wrapped.List(gvk.WorkloadEntry, ""),
			"default/obj",
			"default/obj",
		)
		assertHasObjectsOrdered(t, wrapped.List(gvk.WorkloadEntry, "default"),
			"default/obj",
			"default/obj",
		)
		// Ensure we Get() the real one, not the peered one
		assert.Equal(t, wrapped.Get(gvk.WorkloadEntry, "obj", "default").Labels, nil)
	})
	t.Run("handler", func(t *testing.T) {
		wrapped := setupEmpty(t)
		tt := assert.NewTracker[string](t)
		wrapped.RegisterEventHandler(gvk.WorkloadEntry, func(c config.Config, c2 config.Config, event model.Event) {
			tt.Record(fmt.Sprintf("%s/%s", event, config.NamespacedName(c2).String()))
		})
		createStandard(t, wrapped, "obj", "default")
		tt.WaitOrdered("add/default/obj")
		createPeered(t, wrapped, "obj", "default")
		tt.WaitOrdered("add/default/obj")
		createPeered(t, wrapped, "obj2", "default")
		tt.WaitOrdered("add/default/obj2")
	})
}

func setupBase(t *testing.T) model.ConfigStoreController {
	test.SetForTest(t, &features.EnableEnvoyMultiNetworkHBONE, true)
	store := memory.MakeSkipValidation(collections.Pilot)
	base := memory.NewSyncController(store)
	wrapped := WrapControllerWithPeeringTranslation(base)

	createStandard(t, base, "real-obj", "default")
	createStandard(t, base, "real-obj2", "default")
	createPeered(t, base, "peer-obj1", "default")
	createPeered(t, base, "peer-obj2", "not-default")
	return wrapped
}

func setupEmpty(t *testing.T) model.ConfigStoreController {
	test.SetForTest(t, &features.EnableEnvoyMultiNetworkHBONE, true)
	store := memory.MakeSkipValidation(collections.Pilot)
	base := memory.NewSyncController(store)
	wrapped := WrapControllerWithPeeringTranslation(base)

	return wrapped
}
