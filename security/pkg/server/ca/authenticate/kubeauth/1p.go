// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package kubeauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"istio.io/istio/pkg/env"
)

// Require3PToken disables the use of K8S 1P tokens. Note that 1P tokens can be used to request
// 3P TOKENS. A 1P token is the token automatically mounted by Kubelet and used for authentication with
// the Apiserver.
var Require3PToken = env.Register("REQUIRE_3P_TOKEN", true,
	"Reject k8s default tokens, without audience. If false, default K8S token will be accepted").Get()

// shouldBypassAudienceCheck detects if the token is a K8S unbound token.
// It is a regular JWT with no audience and expiration, which can
// be exchanged with bound tokens with audience.
//
// This is used to determine if we check audience in the token.
// Clients should not use unbound tokens except in cases where
// bound tokens are not possible.
func shouldBypassAudienceCheck(jwt string) bool {
	if Require3PToken {
		return false
	}
	aud, f := extractJwtAud(jwt)
	if !f {
		return false // unbound tokens are valid JWT
	}

	return len(aud) == 0
}

// extractJwtAud extracts the audiences from a JWT token. If aud cannot be parse, the bool will be set
// to false. This distinguishes aud=[] from not parsed.
func extractJwtAud(jwt string) ([]string, bool) {
	jwtSplit := strings.Split(jwt, ".")
	if len(jwtSplit) != 3 {
		return nil, false
	}
	payload := jwtSplit[1]

	payloadBytes, err := decodeJwtPart(payload)
	if err != nil {
		return nil, false
	}

	structuredPayload := jwtPayload{}
	err = json.Unmarshal(payloadBytes, &structuredPayload)
	if err != nil {
		return nil, false
	}

	return structuredPayload.Aud, true
}

type jwtPayload struct {
	// Aud is JWT token audience - used to identify 3p tokens.
	// It is empty for the default K8S tokens.
	Aud []string `json:"aud"`
}

func decodeJwtPart(seg string) ([]byte, error) {
	if l := len(seg) % 4; l > 0 {
		seg += strings.Repeat("=", 4-l)
	}

	return base64.URLEncoding.DecodeString(seg)
}
