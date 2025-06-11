// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package spire

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"istio.io/istio/pkg/test/util/assert"
)

// TestIdentityChangeEvents tests SubscribeToIdentityChange(), UnsubscribeFromIdentityChange() and notifyAll()
func TestIdentityChangeEvents(t *testing.T) {
	w := NewWorkloadClient("")

	numListeners := 10
	lis := make(map[string]<-chan struct{})
	for i := 0; i < numListeners; i++ {
		ch, id := w.SubscribeToIdentityChange()
		lis[id] = ch
	}
	assert.Equal(t, 10, len(w.listeners))

	numEvents := 15
	for i := 0; i < numEvents; i++ {
		w.notifyAll()
	}

	// Multiple events are coalesced such that a
	// listener only acts on the aggregated event
	for _, ch := range lis {
		<-ch
		assert.Equal(t, 0, len(ch))
	}

	for id := range lis {
		w.UnsubscribeFromIdentityChange(id)
	}

	assert.Equal(t, 0, len(w.listeners))
}

func TestX509Watcher(t *testing.T) {
	block, _ := pem.Decode([]byte(testLeafCertPem))
	if block == nil {
		t.Fatalf("failed to parse certificate PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %s", err)
	}

	block, _ = pem.Decode([]byte(testIntermediateCertPem))
	if block == nil {
		t.Fatalf("failed to parse certificate PEM")
	}
	intermediate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse certificate: %s", err)
	}

	block, _ = pem.Decode([]byte(testKeyPem))
	parseResult, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("failed to parse private key: %s", err)
	}
	key := parseResult.(*ecdsa.PrivateKey)

	testSpiffeID := "spiffe://cluster.local/ns/foo/sa/baz"
	spiffeID, _ := spiffeid.FromString(testSpiffeID)

	svids := []*x509svid.SVID{
		{
			ID: spiffeID,
			Certificates: []*x509.Certificate{
				leaf,
				intermediate,
			},
			PrivateKey: key,
		},
	}

	w := NewWorkloadClient("")
	assert.Equal(t, "", w.currentSpiffeID)
	assert.Equal(t, nil, w.GetWorkloadCert())

	w.OnX509ContextUpdate(&workloadapi.X509Context{SVIDs: svids})
	assert.Equal(t, testSpiffeID, w.currentSpiffeID)
	assert.Equal(t, true, w.GetWorkloadCert() != nil)

	noIdentityError := status.Error(codes.PermissionDenied, "no identity issued")
	w.OnX509ContextWatchError(noIdentityError)
	assert.Equal(t, "", w.currentSpiffeID)
	assert.Equal(t, true, w.GetWorkloadCert() == nil)

	w.OnX509ContextUpdate(&workloadapi.X509Context{SVIDs: svids})
	assert.Equal(t, testSpiffeID, w.currentSpiffeID)
	assert.Equal(t, true, w.GetWorkloadCert() != nil)
}

func TestStartX509Watcher(t *testing.T) {
	w := NewWorkloadClient("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.StartX509Watcher(ctx)
	time.Sleep(1 * time.Second)
}

var testLeafCertPem = `-----BEGIN CERTIFICATE-----
MIICGzCCAcGgAwIBAgIQH/4p7B+WprOnZxCaFurSfTAKBggqhkjOPQQDAjAeMQsw
CQYDVQQGEwJVUzEPMA0GA1UEChMGU1BJRkZFMB4XDTIzMDIxNTIyMDExNVoXDTIz
MDIxNzIyMDEyNVowSDELMAkGA1UEBhMCVVMxDjAMBgNVBAoTBVNQSVJFMSkwJwYD
VQQtEyBkZjVjNjI0ZTNhOGRhODM3ZmZjN2E5YmY2MmIwOTE4MTBZMBMGByqGSM49
AgEGCCqGSM49AwEHA0IABC4hnSC9UZk5ZN5MDrFJxPyAQogYfq8JYQilO7AB03Co
kezoE9vDrcvbJGJpUIPbeL20f1vRF3XRkkdiaA07Sr6jgbYwgbMwDgYDVR0PAQH/
BAQDAgOoMB0GA1UdJQQWMBQGCCsGAQUFBwMBBggrBgEFBQcDAjAMBgNVHRMBAf8E
AjAAMB0GA1UdDgQWBBRtrAkRJY5/XZ683nfMnp+X+gkutjAfBgNVHSMEGDAWgBRj
4Q70IauHdi4IkC9ux51ntpOwYDA0BgNVHREELTArhilzcGlmZmU6Ly9jbHVzdGVy
LmxvY2FsL25zL2Nsb3VkL3NhL3ZtdGVzdDAKBggqhkjOPQQDAgNIADBFAiEAhySs
+PLyDNHheEvkn/y9nF9paFqY0N3MNcFj0IeaQz8CIBKctx7EZr0mmuew1N//NFgg
Setg37sK8wyBI9rhR6k0
-----END CERTIFICATE-----`

var testIntermediateCertPem = `-----BEGIN CERTIFICATE-----
MIICfjCCAWagAwIBAgIRALEn7gCmOqHOXEOQvIT76UQwDQYJKoZIhvcNAQELBQAw
GDEWMBQGA1UEChMNY2x1c3Rlci5sb2NhbDAeFw0yMzAyMTUyMjAwMzZaFw0yMzAy
MjIyMjAwNDZaMB4xCzAJBgNVBAYTAlVTMQ8wDQYDVQQKEwZTUElGRkUwWTATBgcq
hkjOPQIBBggqhkjOPQMBBwNCAAQn6MchE74R9XigtLsFxLBZg/jEX0Ix0rrPJFbm
jRCSr3yRlhOri4KPsSFn6f/wUDAagpVmB78RJd1f/X2BBiido4GHMIGEMA4GA1Ud
DwEB/wQEAwIBBjAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRj4Q70IauHdi4I
kC9ux51ntpOwYDAfBgNVHSMEGDAWgBTxMN8kYko1YGWBx8FTrM+fkSA6YjAhBgNV
HREEGjAYhhZzcGlmZmU6Ly9jbHVzdGVyLmxvY2FsMA0GCSqGSIb3DQEBCwUAA4IB
AQAtVfn+qY1KMYmJcGqBFRx1qGHM6obSJYQnnP9Xyon/co5wGY3sNJRKaKOOCBwT
15JCXUj5Lb7YHsmn9PNS2wRxOa+y+PyoT9odkVUKKFSNWOXduYmXMnDIEPeYR088
JV/aRshYuJi3ryNZN94EYo5mEP35kTV4aftlcNrV715/1Y6feoSDhnG7pqA7tDik
iODezcfBGgqJiLV8sXGIhcbyVbXhatOZzD0EftxMk9GQLaDZAf6RD+E87GIK2/Dy
uGA8ZyDMh3jAIhH9pEMEKNd12xNsw5WyIWxLjc/nLaog9G6N+j4RajoVpIbosbVr
uc1kWAx8r1cZa172o2PMLds4
-----END CERTIFICATE-----`

var testKeyPem = `-----BEGIN PRIVATE KEY-----
MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQgiyEnTOvOjwQ9fnZt
GgFjjCumoL4DFULGvVO0aQXq3iKhRANCAAQuIZ0gvVGZOWTeTA6xScT8gEKIGH6v
CWEIpTuwAdNwqJHs6BPbw63L2yRiaVCD23i9tH9b0Rd10ZJHYmgNO0q+
-----END PRIVATE KEY-----`
