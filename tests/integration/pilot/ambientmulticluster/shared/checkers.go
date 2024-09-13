//go:build integ
// +build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package shared

import (
	"fmt"
	"net/http"
	"strings"

	"istio.io/istio/pkg/maps"
	echot "istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/util/sets"
)

var WaypointRemoteDirectLocal = check.And(check.OK(), PerClusterChecker(map[string]echo.Checker{
	LocalCluster:  CheckNotTraversedWaypoint(),
	RemoteCluster: CheckTraversedWaypointIn(RemoteCluster),
}))

var WaypointLocalForBoth = check.And(check.OK(), PerClusterChecker(map[string]echo.Checker{
	LocalCluster:  CheckTraversedWaypointIn(LocalCluster),
	RemoteCluster: CheckTraversedWaypointIn(LocalCluster),
}))

// DestinationWorkload checks the destination workload the request landed on.
func DestinationWorkload(expected string) echo.Checker {
	return check.Each(func(r echot.Response) error {
		if !strings.HasPrefix(r.Hostname, expected+"-") {
			return fmt.Errorf("expected workload %s, received %s", expected, r.Hostname)
		}
		return nil
	})
}

func IsL7() echo.Checker {
	return check.Each(func(r echot.Response) error {
		// TODO: response headers?
		_, f := r.RequestHeaders[http.CanonicalHeaderKey("X-Request-Id")]
		if !f {
			return fmt.Errorf("X-Request-Id not set, is L7 processing enabled?")
		}
		return nil
	})
}

func IsL4() echo.Checker {
	return check.Each(func(r echot.Response) error {
		// TODO: response headers?
		_, f := r.RequestHeaders[http.CanonicalHeaderKey("X-Request-Id")]
		if f {
			return fmt.Errorf("X-Request-Id set, is L7 processing enabled unexpectedly?")
		}
		return nil
	})
}

func PerClusterChecker(checkers map[string]echo.Checker) echo.Checker {
	return func(result echo.CallResult, err error) error {
		responses := map[string]*echo.CallResult{}
		for _, rr := range result.Responses {
			_, f := checkers[rr.Cluster]
			if !f {
				return fmt.Errorf("hit unexpected cluster %q", rr.Cluster)
			}
			if _, f := responses[rr.Cluster]; !f {
				responses[rr.Cluster] = &echo.CallResult{
					From:      result.From,
					Opts:      result.Opts,
					Responses: nil,
				}
			}
			responses[rr.Cluster].Responses = append(responses[rr.Cluster].Responses, rr)
		}
		if !sets.New(maps.Keys(responses)...).Equals(sets.New(maps.Keys(checkers)...)) {
			return fmt.Errorf("expected to hit clusters %v, got %v", maps.Keys(checkers), maps.Keys(responses))
		}
		for c, rr := range responses {
			checker := checkers[c]
			if err := checker.Check(*rr, err); err != nil {
				return fmt.Errorf("per-cluster check for %q: %v", c, err)
			}
		}
		return nil
	}
}

func CheckTraversedWaypointIn(clusters ...string) echo.Checker {
	return check.Each(func(r echot.Response) error {
		want := sets.New(clusters...)
		all := r.RequestHeaders.Values("X-Istio-Clusters")
		if len(want) != len(all) {
			return fmt.Errorf("expected to hit clusters %v, received %s", clusters, all)
		}
		want.DeleteAll(all...)
		if !want.IsEmpty() {
			return fmt.Errorf("expected to hit clusters %v, received %s", clusters, all)
		}
		return nil
	})
}

func CheckNotTraversedWaypoint() echo.Checker {
	return check.RequestHeader("X-Istio-Clusters", "")
}

func CheckPolicyAppliedByWorkload(workload ...string) echo.Checker {
	return check.Each(func(r echot.Response) error {
		want := sets.New(workload...)
		all := r.RequestHeaders.Values("X-Istio-Workload")
		if len(want) != len(all) {
			return fmt.Errorf("expected to have workloads %v, received %s", workload, all)
		}
		for _, v := range all {
			for w := range want {
				if strings.HasPrefix(v, w+"-") {
					want.Delete(w)
				}
			}
		}
		if !want.IsEmpty() {
			return fmt.Errorf("expected to have workloads %v, received %s", workload, all)
		}
		return nil
	})
}

// CheckPolicyEnforced is a pretty specialized check, that asserts policy was run against a given cluster->workload pair.
// The 'required' map is things that MUST be present, while optional is not required.
func CheckPolicyEnforced(required map[string]string, optional map[string]string) echo.Checker {
	return check.Each(func(r echot.Response) error {
		required := maps.Clone(required)
		optional := maps.Clone(optional)
		clusters := r.RequestHeaders.Values("X-Istio-Clusters")
		workloads := r.RequestHeaders.Values("X-Istio-Workload")
		if len(clusters) != len(workloads) {
			return fmt.Errorf("expected to have to hit 1 cluster for each workload: %v/%v", clusters, workloads)
		}
		for i := range clusters {
			c := clusters[i]
			w := workloads[i]
			wasRequired := strings.HasPrefix(w, required[c]+"-")
			wasOptional := strings.HasPrefix(w, optional[c]+"-")
			if !wasRequired && !wasOptional {
				return fmt.Errorf("unexpected pair: %v/%v", c, w)
			}
			if wasRequired {
				delete(required, c)
			}
			if wasOptional {
				delete(optional, c)
			}
		}
		if len(required) != 0 {
			return fmt.Errorf("failed to hit some required pairs: %+v", required)
		}
		return nil
	})
}
