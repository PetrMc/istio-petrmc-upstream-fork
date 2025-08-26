//go:build integ

// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ambient

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"path"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	echoClient "istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/env"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/common/ports"
	"istio.io/istio/pkg/test/framework/resource/config/apply"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/file"
	ingressutil "istio.io/istio/tests/integration/security/sds_ingress/util"
)

// the global config includes an egress waypoint, a ServiceEntry defining the external service
// and an EnvoyFilter to add/remove the principal routing header.
//
//go:embed testdata/solo_mtls_originate/global-config-tmpl.yaml
var globalConfig string

// the app config includes a ServiceEntry, DestinationRule, and HTTPRoute to route to the external service
//
//go:embed testdata/solo_mtls_originate/app-config-tmpl.yaml
var appConfig string

const (
	mtlsOriginateNamespaceName = "solo-mtls-originate-namespace"
	mtlsOriginateGatewayName   = "egress-gateway"
	certsPath                  = "/tests/testdata/certs"
	fakeHost                   = "fake.external.invalid"
)

func testMtlsOriginate(t framework.TestContext) {
	// this is... unfortunate but we want a stable namespace name here
	_, err := t.Clusters().
		Default().
		Kube().
		CoreV1().
		Namespaces().
		Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: mtlsOriginateNamespaceName,
				Labels: map[string]string{
					label.IoIstioUseWaypoint.Name: mtlsOriginateGatewayName,
				},
			},
		}, metav1.CreateOptions{})
	assert.NoError(t, err)
	defer t.CleanupConditionally(func() {
		t.Clusters().
			Default().
			Kube().
			CoreV1().
			Namespaces().
			Delete(context.Background(), mtlsOriginateNamespaceName, metav1.DeleteOptions{})
	})
	// create the global resources to be used for the test
	t.ConfigKube().
		New().
		Eval(mtlsOriginateNamespaceName,
			map[string]any{
				"GatewayName":  mtlsOriginateGatewayName,
				"ExternalHost": fakeHost,
			},
			globalConfig).
		ApplyOrFail(t, apply.CleanupConditionally)
	// only captured apps will obey the waypoints
	for _, app := range apps.MTLSEchoClients {
		setupMTLSEgressForApp(t,
			app,
			mtlsOriginateNamespaceName,
			apps.MockExternal.Config().ClusterLocalFQDN(),
			fakeHost, // this is the global service entry name
		)
	}

	for _, app := range apps.MTLSEchoClients {
		t.NewSubTestf("test %s to %s", app.ServiceName(), fakeHost).Run(func(t framework.TestContext) {
			app.CallOrFail(t, echo.CallOptions{
				Address: fakeHost,
				Port:    ports.HTTP, // we make the call on std http, but the gateway will egress as mTLS
				Check: check.And(
					check.NoErrorAndStatus(http.StatusOK),
					check.Each(func(r echoClient.Response) error {
						expectedClientCertSubject := "CN=" + app.ServiceName()
						if r.ClientCertSubject != expectedClientCertSubject {
							return fmt.Errorf("expected client cert subject to be %s, got %s", expectedClientCertSubject, r.ClientCertSubject)
						}
						return nil
					}),
				),
			})
		})
	}
}

func setupMTLSEgressForApp(t framework.TestContext, app echo.Instance, egressNamespace string, externalRealHost string, globalServiceEntryName string) {
	t.Helper()
	appName := app.ServiceName()
	appNamespace := app.NamespaceName()
	appIdentity := "spiffe://" + app.SpiffeIdentity()
	credentialName := appName + "-client-mtls"
	credentialNamespacedName := appNamespace + "/" + credentialName
	caCert := certsPath + "/unique-apps/" + appName + "-client-ca-cert.pem"
	certChain := certsPath + "/unique-apps/" + appName + "-client-cert.pem"
	key := certsPath + "/unique-apps/" + appName + "-client-key.pem"
	// create the credential
	ingressutil.CreateIngressKubeSecretInNamespace(t, credentialName, ingressutil.Mtls, ingressutil.IngressCredential{
		Certificate: file.AsStringOrFail(t, path.Join(env.IstioSrc, certChain)),
		PrivateKey:  file.AsStringOrFail(t, path.Join(env.IstioSrc, key)),
		CaCert:      file.AsStringOrFail(t, path.Join(env.IstioSrc, caCert)),
	}, false, appNamespace, t.AllClusters()...)

	// create the app resources to be used for the test
	t.ConfigKube().
		New().
		Eval(appNamespace,
			map[string]any{
				"AppName":                appName,
				"Namespace":              appNamespace,
				"AppIdentity":            appIdentity,
				"CredentialName":         credentialNamespacedName,
				"EgressNamespace":        egressNamespace,
				"ExternalHost":           externalRealHost,
				"GlobalServiceEntryName": globalServiceEntryName,
			},
			appConfig).
		ApplyOrFail(t, apply.CleanupConditionally)
}

func TestSoloMtlsOriginate(t *testing.T) {
	framework.NewTest(t).
		Run(func(t framework.TestContext) {
			t.NewSubTest("solo mtls originate").Run(testMtlsOriginate)
		})
}
