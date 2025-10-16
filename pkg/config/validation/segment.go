// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package validation

import (
	"fmt"
	"strings"

	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/validation/agent"
	soloapi "istio.io/istio/soloapi/v1alpha1"
)

// ValidateSegment validates a Segment resource
var ValidateSegment = RegisterValidateFunc("ValidateSegment",
	func(cfg config.Config) (Warning, error) {
		segment, ok := cfg.Spec.(*soloapi.SegmentSpec)
		if !ok {
			return nil, fmt.Errorf("cannot cast to SegmentSpec")
		}

		errs := Validation{}

		// Validate domain
		if segment.Domain == "" {
			errs = AppendValidation(errs, fmt.Errorf("domain cannot be empty"))
		} else {
			// Check for leading or trailing dots
			if strings.HasPrefix(segment.Domain, ".") {
				errs = AppendValidation(errs, fmt.Errorf("domain cannot start with a dot"))
			}
			if strings.HasSuffix(segment.Domain, ".") {
				errs = AppendValidation(errs, fmt.Errorf("domain cannot end with a dot"))
			}

			// Use the existing ValidateFQDN which checks:
			// - Domain length (max 255 chars)
			// - Each label is valid DNS1123 format
			// - Top-level domain is not all-numeric
			if err := agent.ValidateFQDN(segment.Domain); err != nil {
				errs = AppendValidation(errs, err)
			}

			// Check for reserved domain
			if segment.Domain == "mesh.internal" {
				errs = AppendValidation(errs, fmt.Errorf("domain cannot be 'mesh.internal' (reserved)"))
			}
		}

		// Check segment name
		if cfg.Name == "default" {
			errs = AppendValidation(errs, fmt.Errorf("name cannot be 'default' (reserved)"))
		}

		return errs.Unwrap()
	})
