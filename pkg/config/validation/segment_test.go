// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package validation

import (
	"strings"
	"testing"

	"istio.io/istio/pkg/config"
	soloapi "istio.io/istio/soloapi/v1alpha1"
)

func TestValidateSegment(t *testing.T) {
	tests := []struct {
		name        string
		segmentName string
		domain      string
		wantError   bool
		wantWarning bool
		errorMsg    string
	}{
		{
			name:        "valid segment",
			segmentName: "production",
			domain:      "prod.mesh.internal",
			wantError:   false,
		},
		{
			name:        "valid single label domain",
			segmentName: "test",
			domain:      "test",
			wantError:   false,
		},
		{
			name:        "empty domain",
			segmentName: "test",
			domain:      "",
			wantError:   true,
			errorMsg:    "domain cannot be empty",
		},
		{
			name:        "domain with leading dot",
			segmentName: "test",
			domain:      ".mesh.internal",
			wantError:   true,
			errorMsg:    "domain cannot start with a dot",
		},
		{
			name:        "domain with trailing dot",
			segmentName: "test",
			domain:      "mesh.internal.",
			wantError:   true,
			errorMsg:    "domain cannot end with a dot",
		},
		{
			name:        "reserved domain mesh.internal",
			segmentName: "test",
			domain:      "mesh.internal",
			wantError:   true,
			errorMsg:    "domain cannot be 'mesh.internal' (reserved)",
		},
		{
			name:        "reserved name default",
			segmentName: "default",
			domain:      "default.mesh",
			wantError:   true,
			errorMsg:    "name cannot be 'default' (reserved)",
		},
		{
			name:        "domain label too long",
			segmentName: "test",
			domain:      "averylonglabelthatexceedsthemaximumlengthof63characterswhichisnotallowed.mesh",
			wantError:   true,
			errorMsg:    "invalid", // ValidateFQDN will report this as invalid DNS label
		},
		{
			name:        "domain with empty label",
			segmentName: "test",
			domain:      "test..mesh",
			wantError:   true,
			errorMsg:    "invalid", // ValidateFQDN will report invalid DNS label
		},
		{
			name:        "domain starting with hyphen",
			segmentName: "test",
			domain:      "-test.mesh",
			wantError:   true,
			errorMsg:    "invalid", // ValidateFQDN will report invalid DNS label
		},
		{
			name:        "domain ending with hyphen",
			segmentName: "test",
			domain:      "test-.mesh",
			wantError:   true,
			errorMsg:    "invalid", // ValidateFQDN will report invalid DNS label
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{
				Meta: config.Meta{
					Name:      tt.segmentName,
					Namespace: "istio-system",
				},
				Spec: &soloapi.SegmentSpec{
					Domain: tt.domain,
				},
			}

			warning, err := ValidateSegment(cfg)

			if tt.wantError {
				if err == nil {
					t.Errorf("ValidateSegment() expected error containing %q, got nil", tt.errorMsg)
				} else if tt.errorMsg != "" && !contains(err.Error(), tt.errorMsg) {
					t.Errorf("ValidateSegment() error = %v, want error containing %q", err, tt.errorMsg)
				}
			} else if err != nil {
				t.Errorf("ValidateSegment() unexpected error = %v", err)
			}

			if tt.wantWarning {
				if warning == nil {
					t.Errorf("ValidateSegment() expected warning, got nil")
				}
			} else if warning != nil {
				t.Errorf("ValidateSegment() unexpected warning = %v", warning)
			}
		})
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
