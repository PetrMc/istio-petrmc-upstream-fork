// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package istioagent

import (
	"crypto/tls"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	istiogrpc "istio.io/istio/pilot/pkg/grpc"
)

func (p *XdsProxy) buildSPIREIstiodClientDialOpts(sa *Agent) ([]grpc.DialOption, error) {
	tlsOpts, err := p.getTLSOptions(sa)
	if err != nil {
		return nil, fmt.Errorf("failed to get TLS options to talk to upstream: %v", err)
	}

	tlsOpts.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		workloadCert := p.spireClient.GetWorkloadCert()
		if workloadCert == nil {
			return nil, errors.New("workload certificate not available via SPIRE")
		}
		return workloadCert, nil
	}

	options, err := istiogrpc.ClientOptions(nil, tlsOpts)
	if err != nil {
		return nil, err
	}

	return options, nil
}
