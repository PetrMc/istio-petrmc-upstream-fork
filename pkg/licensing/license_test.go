// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package licensing

import (
	"context"
	"testing"

	"github.com/solo-io/licensing/pkg/client"
	"github.com/solo-io/licensing/pkg/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
)

type mockLicenseResponse struct {
	state   client.LicenseState
	product model.Product
}

var licenses = map[string]mockLicenseResponse{
	"expired enterprise": {state: client.LicenseStateExpired, product: model.Product_GlooMesh},
	"active enterprise":  {state: client.LicenseStateOk, product: model.Product_GlooMesh},
	"invalid enterprise": {state: client.LicenseStateInvalid, product: model.Product_GlooMesh},
	"active core":        {state: client.LicenseStateOk, product: model.Product_GlooCore},
	"active trial":       {state: client.LicenseStateOk, product: model.Product_GlooTrial},
	"expired trial":      {state: client.LicenseStateExpired, product: model.Product_GlooTrial},
	"active support":     {state: client.LicenseStateOk, product: model.Product_IstioSupportBasic},
	"expired support":    {state: client.LicenseStateExpired, product: model.Product_IstioSupportBasic},
	"invalid support":    {state: client.LicenseStateInvalid, product: model.Product_IstioSupportBasic},
}

type MockClient struct {
	licenses []string
}

func (m *MockClient) GetLicense(product model.Product) (client.LicenseState, *model.License) {
	for _, l := range m.licenses {
		info, f := licenses[l]
		if !f {
			panic("invalid license")
		}
		if info.product == product {
			return info.state, &model.License{
				Product: product,
			}
		}
	}
	return client.LicenseStateNoLicense, nil
}

func (m *MockClient) AddLicenses(ctx context.Context, licenses []string) error {
	m.licenses = append(m.licenses, licenses...)
	return nil
}

var _ licenseClient = &MockClient{}

func setupTest(kc kubernetes.Interface) (LicenseState, error) {
	mc := &MockClient{}
	licenseState = NewLazy[kubernetes.Interface, LicenseInfo](func(kcp *kubernetes.Interface) (LicenseInfo, error) {
		return checkLicense(func() licenseClient { return mc }, kcp)
	})
	if kc == nil {
		kc = kube.NewFakeClient().Kube()
	}
	return InitializeLicenseState(kc)
}

func TestLicense(t *testing.T) {
	t.Run("active enterprise env", func(t *testing.T) {
		test.SetEnvForTest(t, envInlineLicense, "active enterprise")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("expired enterprise env", func(t *testing.T) {
		// For enterprise, even when expired, it is still valid
		test.SetEnvForTest(t, envInlineLicense, "expired enterprise")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("invalid enterprise env", func(t *testing.T) {
		// For enterprise, invalid is not allowed
		test.SetEnvForTest(t, envInlineLicense, "invalid enterprise")
		state, err := setupTest(nil)
		assert.Error(t, err)
		assert.Equal(t, state, StateInvalid)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), false)
		assert.Equal(t, CheckLicense(FeatureLTS, false), false)
	})
	t.Run("active trial env", func(t *testing.T) {
		test.SetEnvForTest(t, envInlineLicense, "active trial")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("expired trial env", func(t *testing.T) {
		// For trial, even when expired, it is NOT valid
		test.SetEnvForTest(t, envInlineLicense, "expired trial")
		state, err := setupTest(nil)
		assert.Error(t, err)
		assert.Equal(t, state, StateExpired)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), false)
		assert.Equal(t, CheckLicense(FeatureLTS, false), false)
	})
	t.Run("active core", func(t *testing.T) {
		// For core, we allow LTS/FIPS but not other features
		test.SetEnvForTest(t, envInlineLicense, "active core")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), false)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
		assert.Equal(t, CheckLicense(FeatureFIPS, false), true)
	})
	t.Run("active support env", func(t *testing.T) {
		test.SetEnvForTest(t, envInlineLicense, "active support")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("expired support env", func(t *testing.T) {
		// For support, even when expired, it is still valid
		test.SetEnvForTest(t, envInlineLicense, "expired support")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("invalid support env", func(t *testing.T) {
		// For support, invalid is not allowed
		test.SetEnvForTest(t, envInlineLicense, "invalid support")
		state, err := setupTest(nil)
		assert.Error(t, err)
		assert.Equal(t, state, StateInvalid)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), false)
		assert.Equal(t, CheckLicense(FeatureLTS, false), false)
	})
	t.Run("bypass env", func(t *testing.T) {
		// Test builds should always be allowed to bypass
		test.SetEnvForTest(t, envBypassCheck, "true")
		state, err := setupTest(nil)
		assert.NoError(t, err)
		assert.Equal(t, state, StateBypassed)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
	t.Run("kubernetes", func(t *testing.T) {
		kc := kube.NewFakeClient(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "license",
				Namespace: "gloo-system",
			},
			Data: map[string][]byte{
				"license-key": []byte("active enterprise"),
			},
		})
		test.SetEnvForTest(t, envSecretRefLicense, "gloo-system/license")
		state, err := setupTest(kc.Kube())
		assert.NoError(t, err)
		assert.Equal(t, state, StateOK)

		assert.Equal(t, CheckLicense(FeatureMultiCluster, false), true)
		assert.Equal(t, CheckLicense(FeatureLTS, false), true)
	})
}

func TestXDSLicense(t *testing.T) {
	cases := []struct {
		name string
		want LicenseState
	}{
		{"active enterprise", StateOK},
		// Expired enterprise is OK
		{"expired enterprise", StateOK},
		{"invalid enterprise", StateInvalid},
		// Ztunnel currently requires a single value, and all features require enterprise
		{"active core", StateInvalid},
		{"active trial", StateOK},
		// Expire trial is not allowed
		{"expired trial", StateExpired},
		{"active support", StateOK},
		// Expired enterprise is OK
		{"expired support", StateOK},
		{"invalid support", StateInvalid},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			test.SetEnvForTest(t, envInlineLicense, tt.name)
			_, _ = setupTest(nil)
			got := GetXDSLicenseState()
			assert.Equal(t, tt.want, got)
		})
	}
}
