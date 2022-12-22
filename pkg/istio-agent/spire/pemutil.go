// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package spire

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
)

func encodePKCS8PrivateKey(privateKey interface{}) ([]byte, error) {
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: keyBytes,
	}), nil
}

func encodeCertificates(certs []*x509.Certificate) []byte {
	var buf bytes.Buffer
	for _, cert := range certs {
		encodeCertificate(&buf, cert)
	}
	return buf.Bytes()
}

func encodeCertificate(buf *bytes.Buffer, cert *x509.Certificate) {
	// encoding to a memory buffer should not error out
	_ = pem.Encode(buf, &pem.Block{
		Type:  "CERTIFICATE",
		Bytes: cert.Raw,
	})
}
