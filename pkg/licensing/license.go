// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package licensing

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/solo-io/go-utils/contextutils"
	"github.com/solo-io/licensing/pkg/client"
	"github.com/solo-io/licensing/pkg/model"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"istio.io/istio/pkg/env"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/monitoring"
	"istio.io/istio/pkg/util/sets"
	istioversion "istio.io/istio/pkg/version"
)

// IsLts defines if the current build is LTS or not.
// This is a build-time parameter, hence the const; changing it is done by changing the code when the branch goes out of
// upstream support.
const IsLts = false

var licenseState = NewLazy(defaultCheckLicense)

func InitializeLicenseState(kc kubernetes.Interface) (LicenseState, error) {
	if err := licenseState.SetInput(kc); err != nil {
		return StateInvalid, err
	}
	st, err := licenseState.Get()
	return st.State, err
}

func SetForTest() {
	_ = os.Setenv(envBypassCheck, "true")
}

// GetXDSLicenseState returns the current state of the license, as returned over XDS.
// The current XDS interface only allows sending a single string, so doesn't have nuance like "enterprise expired" vs "trial expired".
func GetXDSLicenseState() LicenseState {
	state, _ := licenseState.Get()
	if state.Product == model.Product_GlooCore {
		// Ztunnel isn't product-aware, but all features currently require enterprise.
		// Mark GMC as 'invalid'
		return StateInvalid
	}
	return state.State
}

// CheckLicense checks the license is valid for the given feature.
// A valid license is one that is not expired.
// The verbose flag configures whether warnings will be spammed if it's not able to be used. This should be used
// only when a feature has been explicitly enabled, not implicitly.
func CheckLicense(name LicenseFeature, verbose bool) bool {
	info, err := licenseState.Get()
	state := info.State
	valid := (state == StateBypassed || state == StateOK) && err == nil
	if valid {
		if !glooMeshCoreLicenses.Contains(name) && info.Product == model.Product_GlooCore {
			if verbose {
				message := fmt.Sprintf("SKIPPING FEATURE %s due to licensing issue: detected Gloo Mesh Core license, but feature requires enterprise", name)
				for range 10 {
					// Log 10x for good measure
					log.Error(message)
				}
			}
			return false
		}
		return true
	}
	reason := "expired"
	if err != nil {
		reason = err.Error()
	}
	if verbose {
		message := fmt.Sprintf("SKIPPING FEATURE %s due to licensing issue: %s", name, reason)
		for range 10 {
			// Log 10x for good measure
			log.Error(message)
		}
	}
	return false
}

type LicenseFeature string

const (
	FeatureMultiCluster LicenseFeature = "MultiCluster"
	FeatureECS          LicenseFeature = "ECS"
	FeatureEnvoyFilter  LicenseFeature = "EnvoyFilter"
	FeatureFIPS         LicenseFeature = "FIPS"
	FeatureLTS          LicenseFeature = "LTS"
)

var glooMeshCoreLicenses = sets.New[LicenseFeature](
	FeatureFIPS,
	FeatureLTS,
)

type LicenseState string

const (
	StateUnset    LicenseState = "UNSET"
	StateInvalid  LicenseState = "INVALID"
	StateExpired  LicenseState = "EXPIRED"
	StateBypassed LicenseState = "BYPASSED"
	StateOK       LicenseState = "OK"
)

const (
	// envBypassCheck allows skipping license checks, if it is a dev build
	envBypassCheck      = "SKIP_LICENSE_CHECK"
	envInlineLicense    = "SOLO_LICENSE_KEY"
	envSecretRefLicense = "SOLO_LICENSE_SECRET"
)

var LicenseFailOpen = env.Register("SOLO_LICENSE_FAIL_OPEN",
	false,
	"When fail-open is enabled, an invalid license key will continue to process but with license-gated features disabled. "+
		"When disabled, Istiod will fail to startup.").Get()

type LicenseInfo struct {
	State   LicenseState
	Product model.Product
}

func invalidLicenseInfo(state LicenseState) LicenseInfo {
	return LicenseInfo{State: state}
}

type licenseClient interface {
	GetLicense(product model.Product) (client.LicenseState, *model.License)
	AddLicenses(ctx context.Context, licenses []string) error
}

func foundLicenseForState(lc licenseClient, state client.LicenseState, products ...model.Product) *model.License {
	for _, product := range products {
		licenseState, license := lc.GetLicense(product)
		if licenseState == state {
			return license
		}
	}
	return nil
}

func defaultCheckLicense(kcp *kubernetes.Interface) (LicenseInfo, error) {
	clientBuilder := func() licenseClient {
		return client.NewLicensingClient("", "", monitoring.PrometheusRegisterer)
	}
	return checkLicense(clientBuilder, kcp)
}

// checkLicense asserts that a valid license is found for Gloo.
// Additionally, it returns if the license is not expired.
func checkLicense(clientBuilder func() licenseClient, kcp *kubernetes.Interface) (LicenseInfo, error) {
	logger := zap.S()

	devBuild := istioversion.Info.Version == "unknown" || // Did not build with build info
		istioversion.Info.Version == istioversion.Info.GitRevision || // Built without version set (i.e. local build or test build
		strings.Contains(istioversion.Info.Version, "-dev") || // Built as a pre-release
		strings.Contains(istioversion.Info.Version, "-alpha.") // Built as a pre-release
	if devBuild && os.Getenv(envBypassCheck) != "" {
		return LicenseInfo{State: StateBypassed, Product: model.Product_GlooMesh}, nil
	}

	ctx := contextutils.WithExistingLogger(context.Background(), logger)

	licenses := []string{}
	inline, inlineF := os.LookupEnv(envInlineLicense)
	if inlineF {
		licenses = append(licenses, inline)
	}
	ref, refF := os.LookupEnv(envSecretRefLicense)
	if refF {
		ns, name, ok := strings.Cut(ref, "/")
		if !ok {
			return invalidLicenseInfo(StateInvalid), fmt.Errorf("license reference %v invalid: %v", envSecretRefLicense, ref)
		}
		if kcp == nil {
			return invalidLicenseInfo(StateInvalid), fmt.Errorf("kube client not initialized")
		}
		scrt, err := getLicense(*kcp, ns, name)
		if err != nil {
			return invalidLicenseInfo(StateInvalid), fmt.Errorf("failed to read license from secret: %v", err)
		}
		// The license client only reads from env var or file right now, so hack this in.
		licenses = append(licenses, scrt)
	}
	if len(licenses) == 0 {
		return invalidLicenseInfo(StateUnset), fmt.Errorf("license key was not set")
	}
	lc := clientBuilder()
	if err := lc.AddLicenses(ctx, licenses); err != nil {
		return invalidLicenseInfo(StateInvalid), fmt.Errorf("licenses could not be loaded: %v", err)
	}
	mainGlooProducts := []model.Product{model.Product_GlooMesh, model.Product_GlooCore, model.Product_GlooTrial}
	license := foundLicenseForState(lc, client.LicenseStateOk, mainGlooProducts...)
	if license != nil {
		return LicenseInfo{State: StateOK, Product: license.Product}, nil
	}
	license = foundLicenseForState(lc, client.LicenseStateExpired, model.Product_GlooMesh, model.Product_GlooCore)
	if license != nil {
		// For non-trial licenses, we let them expire but spam them with logs.
		// We lie about a grace period; we may have one eventually but for now its "forever"
		for range 25 {
			log.Errorf("WARNING: found expired license key, temporarily allowing for a grace period")
		}
		return LicenseInfo{State: StateOK, Product: license.Product}, nil
	}
	license = foundLicenseForState(lc, client.LicenseStateExpired, mainGlooProducts...)
	if license != nil {
		return LicenseInfo{State: StateExpired, Product: license.Product}, fmt.Errorf("licenses found but expired: %v", license)
	}
	license = foundLicenseForState(lc, client.LicenseStateInvalid, mainGlooProducts...)
	if license != nil {
		return LicenseInfo{State: StateInvalid, Product: license.Product}, fmt.Errorf("licenses found but invalid: %v", license)
	}
	return invalidLicenseInfo(StateInvalid), fmt.Errorf("no license found")
}

func getLicense(kc kubernetes.Interface, ns string, name string) (string, error) {
	scrt, err := kc.CoreV1().Secrets(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	d := scrt.Data
	if v, f := d["license-key"]; f {
		return string(v), nil
	}
	if v, f := d["gloo-mesh-license-key"]; f {
		return string(v), nil
	}
	if v, f := d["gloo-trial-license-key"]; f {
		return string(v), nil
	}
	return "", fmt.Errorf("no license found in secret (have keys: %v)", maps.Keys(d))
}

// ValidateLTS returns an error if the current build is an LTS build and LTS support is not licensed.
func ValidateLTS() error {
	if !IsLts {
		return nil
	}
	if !CheckLicense(FeatureLTS, false) {
		return fmt.Errorf("this build is an LTS build, which requires a license. However, a valid license has not been detected")
	}
	return nil
}

// ValidateFIPS returns an error if the current build is a FIPS build and FIPS support is not licensed.
func ValidateFIPS() error {
	if !IsBoring() {
		return nil
	}
	if !CheckLicense(FeatureLTS, false) {
		return fmt.Errorf("this build is a FIPS build, which requires a license. However, a valid license has not been detected")
	}
	return nil
}
