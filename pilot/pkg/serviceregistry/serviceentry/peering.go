// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package serviceentry

import (
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/platform/discovery/peering"
)

// PeeringNamespaceWrapper wraps a standard Controller, and transparently handles "Peering" objects.
// Peered objects live in a central namespace, but are logically a part of other namespaces -- essentially they are projected
// into those namespaces.
// These objects have a name and namespace label indicating their respective desired name/namespaces.
type PeeringNamespaceWrapper struct {
	inner model.ConfigStoreController
}

func (w PeeringNamespaceWrapper) Schemas() collection.Schemas {
	return w.inner.Schemas()
}

func (w PeeringNamespaceWrapper) Get(typ config.GroupVersionKind, name, namespace string) *config.Config {
	if namespace == "" || namespace == peering.PeeringNamespace {
		return maybeChangeNamespacePtr(w.inner.Get(typ, name, namespace))
	}
	if normal := maybeChangeNamespacePtr(w.inner.Get(typ, name, namespace)); normal != nil {
		return normal
	}
	// Try to find it in the peering namespace
	if peered := maybeChangeNamespacePtr(w.inner.Get(typ, name, peering.ParentServiceNamespaceLabel)); peered != nil {
		// Make sure it's really the requested namespace
		if peered.Namespace == namespace {
			return peered
		}
	}
	return nil
}

func maybeChangeNamespacePtr(obj *config.Config) *config.Config {
	if !peering.IsPeerObject(obj) {
		return obj
	}
	obj.Namespace = obj.Labels[peering.ParentServiceNamespaceLabel]
	return obj
}

func maybeChangeNamespace(obj config.Config) config.Config {
	return *maybeChangeNamespacePtr(&obj)
}

func (w PeeringNamespaceWrapper) List(typ config.GroupVersionKind, namespace string) []config.Config {
	if namespace == "" || namespace == peering.PeeringNamespace {
		return slices.Map(w.inner.List(typ, namespace), maybeChangeNamespace)
	}
	base := w.inner.List(typ, namespace)
	return append(base, MapFilterInPlace(w.inner.List(typ, peering.PeeringNamespace), func(obj config.Config) (config.Config, bool) {
		if !peering.IsPeerObject(&obj) {
			return config.Config{}, false
		}
		peered := maybeChangeNamespace(obj)
		// Make sure it's really the requested namespace
		if peered.Namespace == namespace {
			return peered, true
		}
		return config.Config{}, false
	})...)
}

func (w PeeringNamespaceWrapper) Create(config config.Config) (revision string, err error) {
	return w.inner.Create(config)
}

func (w PeeringNamespaceWrapper) Update(config config.Config) (newRevision string, err error) {
	return w.inner.Update(config)
}

func (w PeeringNamespaceWrapper) UpdateStatus(config config.Config) (newRevision string, err error) {
	return w.inner.UpdateStatus(config)
}

func (w PeeringNamespaceWrapper) Delete(typ config.GroupVersionKind, name, namespace string, resourceVersion *string) error {
	return w.inner.Delete(typ, name, namespace, resourceVersion)
}

func (w PeeringNamespaceWrapper) RegisterEventHandler(kind config.GroupVersionKind, handler model.EventHandler) {
	w.inner.RegisterEventHandler(kind, func(old config.Config, now config.Config, event model.Event) {
		handler(maybeChangeNamespace(old), maybeChangeNamespace(now), event)
	})
}

func (w PeeringNamespaceWrapper) Run(stop <-chan struct{}) {
	w.inner.Run(stop)
}

func (w PeeringNamespaceWrapper) HasSynced() bool {
	return w.inner.HasSynced()
}

func WrapControllerWithPeeringTranslation(controller model.ConfigStoreController) model.ConfigStoreController {
	if !features.EnableEnvoyMultiNetworkHBONE {
		return controller
	}
	return PeeringNamespaceWrapper{controller}
}

// MapFilterInPlace runs f() over all elements in s and returns any non-nil results
func MapFilterInPlace[E any](s []E, f func(E) (E, bool)) []E {
	n := 0
	for _, x := range s {
		mapped, keep := f(x)
		if keep {
			s[n] = mapped
			n++
		}
	}

	clear(s[n:]) // zero/nil out the obsolete elements, for GC
	return s[:n]
}

// IsPeerObject checks if an object is a peer object which should be logically considered a part of another namespace.
func IsPeerObject(c controllers.Object) bool {
	if c.GetNamespace() != peering.PeeringNamespace {
		return false
	}
	if _, f := c.GetLabels()[peering.ParentServiceLabel]; !f {
		return false
	}
	if _, f := c.GetLabels()[peering.ParentServiceNamespaceLabel]; !f {
		return false
	}
	return true
}
