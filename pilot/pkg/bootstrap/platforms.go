// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package bootstrap

import (
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/leaderelection"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/adsc"
	"istio.io/istio/pkg/backoff"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/licensing"
	"istio.io/istio/pkg/version"
	"istio.io/istio/platform/discovery/peering"
)

func (s *Server) initPlatformIntegrations(args *PilotArgs) {
	s.initPeeringDiscovery(args)
}

func (s *Server) initPeeringDiscovery(args *PilotArgs) {
	if !features.EnablePeering {
		return
	}
	if !licensing.CheckLicense(licensing.FeatureMultiCluster, features.EnablePeeringExplicitly) {
		return
	}
	buildConfig := func(clientName string) *adsc.DeltaADSConfig {
		return &adsc.DeltaADSConfig{
			Config: adsc.Config{
				ClientName: clientName,
				Namespace:  args.Namespace,
				Workload:   args.PodName,
				Revision:   args.Revision,
				Meta: model.NodeMetadata{
					ClusterID:    args.RegistryOptions.KubeOptions.ClusterID,
					IstioVersion: version.Info.Version,
					// To reduce transported data if upstream server supports. Especially for custom servers.
					IstioRevision: args.Revision,
					// Hack to make `istioctl ps` show us... otherwise it is filtered
					ProxyConfig: &model.NodeMetaProxyConfig{},
				}.ToStruct(),
				GrpcOpts: []grpc.DialOption{
					args.KeepaliveOptions.ConvertToClientOption(),
					// Because we use the custom grpc options for adsc, here we should
					// explicitly set transport credentials.
					// TODO: maybe we should use the tls settings within ConfigSource
					// to secure the connection between istiod and remote xds server.
					// We currently are NOT doing TLS. This is because the server side will require auth for TLS...
					// grpc.WithTransportCredentials(insecure.NewCredentials()),
					grpc.WithTransportCredentials(credentials.NewTLS(
						&tls.Config{
							// Always skip verifying for now. We don't have their root cert. Else we need a way to sync that alongside
							// the Gateway object, which is a bit problematic.
							// nolint: gosec
							// The lint is valid, we need to fix this.
							InsecureSkipVerify: true,
							GetClientCertificate: func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
								cert, err := s.getIstiodCertificate(nil)
								if err != nil {
									return nil, err
								}
								return cert, nil
							},
						})),
				},
				BackoffPolicy: backoff.NewExponentialBackOff(backoff.DefaultOption()),
			},
		}
	}

	s.addStartFunc("networking peering", func(stop <-chan struct{}) error {
		go leaderelection.
			NewLeaderElection(args.Namespace, args.PodName, leaderelection.PeeringController, args.Revision, s.kubeClient).
			AddRunFunction(func(leaderStop <-chan struct{}) {
				if s.kubeClient.CrdWatcher().WaitForCRD(gvr.KubernetesGateway, stop) {
					if s.statusManager == nil {
						s.initStatusManager(args)
					}

					c := peering.New(
						s.kubeClient,
						args.RegistryOptions.KubeOptions.ClusterID.String(),
						args.RegistryOptions.KubeOptions.DomainSuffix,
						buildConfig,
						args.KrtDebugger,
						s.environment.Watcher,
						s.statusManager,
					)
					// Start informers again. This fixes the case where informers for namespace do not start,
					// as we create them only after acquiring the leader lock
					// Note: stop here should be the overall pilot stop, NOT the leader election stop. We are
					// basically lazy loading the informer, if we stop it when we lose the lock we will never
					// recreate it again.
					s.kubeClient.RunAndWait(leaderStop)
					c.Run(leaderStop)
				}
			}).Run(stop)
		return nil
	})
}
