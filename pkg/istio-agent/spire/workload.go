// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package spire

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	istioLog "istio.io/istio/pkg/log"
)

const clientRetryInterval = 5 * time.Second

var log = istioLog.RegisterScope("spire", "SPIRE Workload API client")

// WorkloadClient implements the client interface for the SPIFFE Workload API
type WorkloadClient struct {
	socketPath       string
	workloadx509Cert *tls.Certificate
	mu               sync.RWMutex
	currentSpiffeID  string
	listeners        map[string]chan struct{}
	listenerMu       sync.Mutex
}

// NewWorkloadClient returns a new WorkloadClient instance
func NewWorkloadClient(socketPath string) *WorkloadClient {
	return &WorkloadClient{
		socketPath: socketPath,
		listeners:  make(map[string]chan struct{}),
	}
}

// SubscribeToIdentityChange returns a channel on which subscribed listeners
// receive identity change events, and a subscription ID
func (c *WorkloadClient) SubscribeToIdentityChange() (<-chan struct{}, string) {
	// Use a buffered channel of size 1 since we only use this channel to
	// relay an identity change event. Multiple events are coalesced such
	// that a listener only needs to process a single event if there is a batch of events.
	ch := make(chan struct{}, 1)
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()

	id := uuid.NewString()
	c.listeners[id] = ch
	return ch, id
}

// UnsubscribeFromIdentityChange unsubscribes an existing subscription given its ID
func (c *WorkloadClient) UnsubscribeFromIdentityChange(id string) {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()

	if _, ok := c.listeners[id]; !ok {
		log.Errorf("listener %s not found", id)
		return
	}
	delete(c.listeners, id)
}

// GetWorkloadCert returns the workload's x509 certificate
func (c *WorkloadClient) GetWorkloadCert() *tls.Certificate {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workloadx509Cert
}

// StartX509Watcher starts the SPIFFE x509 SVID watcher.
// If it cannot connect to the Workload API server on the given
// Unix Domain Socket path, it retries indefinitely.
// The watcher terminates when Cancel() is invoked on the given Context.
func (c *WorkloadClient) StartX509Watcher(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Infof("SPIRE workload client received signal to stop, exiting")
			return

		default:
			client, err := workloadapi.New(ctx, workloadapi.WithAddr(c.socketPath))
			if err != nil {
				log.Errorf("error creating SPIRE workload API client: %s, retrying in %t", err, clientRetryInterval)
				time.Sleep(clientRetryInterval)
				continue
			}
			defer client.Close()

			var wg sync.WaitGroup
			wg.Add(1) // for the x509 watcher
			go func() {
				defer wg.Done()
				err := client.WatchX509Context(ctx, c)
				if err != nil && status.Code(err) != codes.Canceled {
					log.Errorf("error watching SPIRE X.509 context: %s", err)
					return
				}
			}()

			// wait for the watcher to terminate
			wg.Wait()
		}
	}
}

// OnX509ContextUpdate is invoked on X509Context updates
func (c *WorkloadClient) OnX509ContextUpdate(certCtx *workloadapi.X509Context) {
	log.Debug("workload x509 context update")
	svid := certCtx.DefaultSVID()
	if svid == nil {
		log.Error("unexpected: did not get x509 SVID")
		return
	}

	cert, err := certificateFromSVID(svid)
	if err != nil {
		log.Errorf("error converting x509 SVID to TLS certificate: %w", err)
		return
	}

	log.Debug("updating workload x509 cert")
	c.mu.Lock()
	c.workloadx509Cert = cert
	shouldNotify := false
	// The SPIFFE ID for the workload changed, notify interested clients
	if c.currentSpiffeID != "" && c.currentSpiffeID != svid.ID.String() {
		log.Info("workload identity changed")
		shouldNotify = true
	}
	c.currentSpiffeID = svid.ID.String()
	c.mu.Unlock()

	if shouldNotify {
		c.notifyAll()
	}
}

func (c *WorkloadClient) notifyAll() {
	c.listenerMu.Lock()
	defer c.listenerMu.Unlock()

	log.Debug("checking for listeners to notify")
	for id, lis := range c.listeners {
		log.Debugf("found listener %s to notify", id)
		// Multiple events are coalesced such that a listener only needs to process
		// a single event if there is a batch of events.
		if len(lis) == 0 {
			log.Infof("notifying listener %s of identity change", id)
			lis <- struct{}{}
		}
	}
}

// OnX509ContextWatchError is invoked on X509Context errors
func (c *WorkloadClient) OnX509ContextWatchError(err error) {
	if status.Code(err) == codes.Canceled {
		log.Warn("SPIRE gRPC canceled")
		return
	}

	// Okay to error if the SVID is not available yet
	log.Debugf("OnX509ContextWatchError error: %s", err)

	// The workload no longer has an identity associated so
	// remove the existing identity mapping
	if status.Code(err) == codes.PermissionDenied {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.currentSpiffeID != "" {
			log.Info("workload identity revoked")
			c.currentSpiffeID = ""
			c.workloadx509Cert = nil
		} else {
			log.Info("workload identity not provisioned")
		}
	}
}

// certificateFromSVID converts an SVID to a tls.Certificate
func certificateFromSVID(svid *x509svid.SVID) (*tls.Certificate, error) {
	if svid == nil {
		return nil, nil
	}

	certificates := svid.Certificates
	if len(certificates) == 0 {
		return nil, errors.New("no certificates found")
	}

	keyPEM, err := encodePKCS8PrivateKey(svid.PrivateKey)
	if err != nil {
		return nil, err
	}

	certsPEM := encodeCertificates(certificates)

	certificate, err := tls.X509KeyPair(certsPEM, keyPEM)
	return &certificate, err
}
