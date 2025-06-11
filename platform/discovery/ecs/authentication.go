// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

package ecs

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"google.golang.org/grpc/metadata"
	v1 "k8s.io/api/core/v1"

	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/security"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/spiffe"
)

// AwsIAMAuthenticator authenticates AWS workloads based on IAM credentials.
// Currently, this is validated to work on ECS.
// This is based on the `iam` method in https://developer.hashicorp.com/vault/docs/auth/aws.
//
// This differs from the more typical approach to workload attestation on AWS (the `ec2` method in vault,
// spire, etc), which uses the signed Instance identity documents. ECS does not have IIDs, so we need an alternative approach.
//
// The ECS metadata server does provide access to the `AccessKeyId`, `SecretAccessKey`, `Token` values which we can use.
// A call to `GetCallerIdentity` can be made to verify these and get the execution role:
//
//	  auth="$(curl -s "http://169.254.170.2$AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")"
//		curl -s -X POST "https://sts.us-west-2.amazonaws.com/" \
//		  --aws-sigv4 aws:amz:us-west-2:sts \
//		  -d "Action=GetCallerIdentity&Version=2011-06-15" \
//		  --user "$(<<<"$auth" jq .AccessKeyId -r):$(<<<"$auth" jq .SecretAccessKey -r)" \
//		  -H "x-amz-security-token: $(<<<"$auth" jq .Token -r)" \
//		  -H "Accept: application/json" \
//		  | jq
//
// The problem here is we don't want to just send these values over to Istiod -- the token is unscoped,
// so would give Istiod extremely high access. In other authentication methods we downscope the token
// to our specific audience, ensuring the token has no privileges beyond authenticating to Istiod.
// AWS doesn't (seem to?) have a method to do this directly, but we can indirectly by utilizing signed requests.
//
// As shown in the `curl` example above, we sign the request to `GetCallerIdentity` with our SecretAccessKey,
// but don't actually include it in the request (note: the `--user` there is configuring the `--aws-sigv4` flag, it is not
// just added directly to a header in plaintext).
// The trick here is instead of sending this request from Ztunnel, we build a signed request, encode it,
// and send it to Istiod. Istiod itself sends the signed request to AWS and gets back the caller identity.
//
// This is secure because:
//   - Ztunnel cannot make a signed request for any identity which it doesn't have access to the
//     role authentication info (token, etc).
//   - Istiod cannot do anything with the information it gets beyond calling GetCallerIdentity.
type AwsIAMAuthenticator struct {
	meshConfig mesh.Holder
	httpClient *http.Client
	sas        kclient.Client[*v1.ServiceAccount]
	roleIndex  kclient.Index[Role, *v1.ServiceAccount]
}

func NewEcsAuthenticator(meshConfig mesh.Holder, client kube.Client) *AwsIAMAuthenticator {
	sas := kclient.NewFiltered[*v1.ServiceAccount](client, kclient.Filter{
		ObjectFilter: client.ObjectFilter(),
	})
	// Add an Index on the pods, storing the service account and node. This allows us to later efficiently query.
	index := kclient.CreateIndex[Role, *v1.ServiceAccount](sas, "ecs-authenticator", func(sa *v1.ServiceAccount) []Role {
		var roles []Role
		if r, f := sa.Annotations[AwsRoleArnAnnotation]; f {
			roles = append(roles, normalizeRoleARN(r))
		}
		if r, f := sa.Annotations[SoloRoleArnAnnotation]; f {
			roles = append(roles, normalizeRoleARN(r))
		}
		return roles
	})
	return &AwsIAMAuthenticator{
		meshConfig: meshConfig,
		roleIndex:  index,
		sas:        sas,
		httpClient: &http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

const (
	// AwsRoleArnAnnotation can be set on a ServiceAccount to bind it to a specific Role.
	// This annotation already exists and is used for some of their workload identity stuff, so respect it to avoid users
	// needing to set it twice.
	AwsRoleArnAnnotation = "eks.amazonaws.com/role-arn"
	// SoloRoleArnAnnotation can be set on a ServiceAccount to bind it to a specific Role.
	SoloRoleArnAnnotation = "ecs.solo.io/role-arn"
)

var _ security.Authenticator = &AwsIAMAuthenticator{}

func (a *AwsIAMAuthenticator) AuthenticatorType() string {
	return "AwsIAMAuthenticator"
}

// Authenticate authenticates the call using the K8s JWT from the context.
// The returned Caller.Identities is in SPIFFE format.
func (a *AwsIAMAuthenticator) Authenticate(authRequest security.AuthContext) (*security.Caller, error) {
	if authRequest.GrpcContext != nil {
		return a.authenticateGrpc(authRequest.GrpcContext)
	}
	if authRequest.Request != nil {
		return a.authenticateHTTP(authRequest.Request)
	}
	return nil, nil
}

func (a *AwsIAMAuthenticator) authenticateHTTP(req *http.Request) (*security.Caller, error) {
	targetJWT, err := ExtractAWSTokenRequest(req)
	if err != nil {
		return nil, fmt.Errorf("target JWT extraction error: %v", err)
	}

	return a.authenticate(targetJWT)
}

func (a *AwsIAMAuthenticator) authenticateGrpc(ctx context.Context) (*security.Caller, error) {
	targetJWT, err := ExtractAWSToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("target JWT extraction error: %v", err)
	}

	return a.authenticate(targetJWT)
}

// StsRequest contains a serialized and signed request from a peer we should forward to AWS
type StsRequest struct {
	Headers map[string]string `json:"headers"`
	Token   string            `json:"token"`
	Host    string            `json:"host"`
}

func (a *AwsIAMAuthenticator) authenticate(request string) (*security.Caller, error) {
	// The token will come in a base64 encoded StsRequest JSON.
	rj, err := base64.StdEncoding.DecodeString(request)
	if err != nil {
		return nil, err
	}
	req := StsRequest{}
	if err := json.Unmarshal(rj, &req); err != nil {
		return nil, err
	}

	// Make sure they are not trying to send us to some unexpected host.
	if !strings.HasPrefix(req.Host, "sts.") && !strings.HasPrefix(req.Host, ".amazonaws.com") {
		return nil, fmt.Errorf("invalid host: %s", req.Host)
	}
	hreq, err := http.NewRequest(http.MethodPost, "https://"+req.Host, bytes.NewBufferString("Action=GetCallerIdentity&Version=2011-06-15"))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		hreq.Header.Set(k, v)
	}
	hreq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hreq.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}

	cresp := GetCallerIdentityResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&cresp); err != nil {
		return nil, fmt.Errorf("failed to decode JSON: %v", err)
	}
	rawArn := cresp.GetCallerIdentityResponse.GetCallerIdentityResult.Arn
	arn, err := parseRoleARN(rawArn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ARN: %v", err)
	}

	// Find the SA associated with the ARN, if any
	sas := a.roleIndex.Lookup(Role(arn))
	if len(sas) == 0 {
		return nil, fmt.Errorf("found role %v, but it does not have any service accounts associated with it", arn)
	}

	identities := slices.Map(sas, func(e *v1.ServiceAccount) string {
		return spiffe.MustGenSpiffeURI(a.meshConfig.Mesh(), e.GetNamespace(), e.GetName())
	})
	return &security.Caller{
		AuthSource: security.AuthSourceSignedRequest,
		Identities: identities,
	}, nil
}

type GetCallerIdentityResponse struct {
	GetCallerIdentityResponse struct {
		GetCallerIdentityResult struct {
			Account string `json:"Account"`
			Arn     string `json:"Arn"`
			UserID  string `json:"UserId"`
		} `json:"GetCallerIdentityResult"`
		ResponseMetadata struct {
			RequestID string `json:"RequestId"`
		} `json:"ResponseMetadata"`
	} `json:"GetCallerIdentityResponse"`
}

type Role string

func (r Role) String() string {
	return string(r)
}

const (
	authorizationMeta = "authorization"
	AWSTokenPrefix    = "AWS "
)

func ExtractAWSToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", fmt.Errorf("no metadata is attached")
	}

	authHeader, exists := md[authorizationMeta]
	if !exists {
		return "", fmt.Errorf("no HTTP authorization header exists")
	}

	for _, value := range authHeader {
		if strings.HasPrefix(value, AWSTokenPrefix) {
			return strings.TrimPrefix(value, AWSTokenPrefix), nil
		}
	}

	return "", fmt.Errorf("no AWS token exists in HTTP authorization header")
}

func ExtractAWSTokenRequest(req *http.Request) (string, error) {
	value := req.Header.Get(authorizationMeta)
	if value == "" {
		return "", fmt.Errorf("no HTTP authorization header exists")
	}

	if strings.HasPrefix(value, AWSTokenPrefix) {
		return strings.TrimPrefix(value, AWSTokenPrefix), nil
	}

	return "", fmt.Errorf("no AWS token exists in HTTP authorization header")
}

func parseRoleARN(a string) (string, error) {
	// iamArn should look like one of the following:
	// 1. arn:aws:iam::<account_id>:<entity_type>/<UserName>
	// 2. arn:aws:sts::<account_id>:assumed-role/<RoleName>/<RoleSessionName>
	// if we get something like 2, then we want to transform that back to what
	// most people would expect, which is arn:aws:iam::<account_id>:role/<RoleName>
	arn, err := arn.Parse(a)
	if err != nil {
		return "", fmt.Errorf("failed to parse arn %q: %v", a, err)
	}
	res := strings.Split(arn.Resource, "/")
	if len(res) != 3 {
		return "", fmt.Errorf("failed to parse arn %q: invalid format", a)
	}
	if arn.Service == "sts" && res[0] == "assumed-role" {
		arn.Service = "iam"
		arn.Resource = "role/" + res[1]
	} else {
		return "", fmt.Errorf("currently only assumed-role is supported")
	}
	return arn.String(), nil
}

// normalizeRoleARN converts a role into its normalized form.
// A Role can have a 'path', which becomes part of the arn. Like `/path/to/role`.
// The final portion, the role name, is unique to the account -- regardless of path.
// When we get an authentication request, we lose the path (see parseRoleARN), so this function strips it as well
// so they can be compared.
func normalizeRoleARN(a string) Role {
	arn, err := arn.Parse(a)
	if err != nil {
		return Role(a)
	}
	segments := strings.Split(arn.Resource, "/")
	if len(segments) < 2 {
		return Role(a)
	}
	if segments[0] != "role" {
		// Unknown format
		return Role(a)
	}
	// Strip of the path, anything between `role/.../<rolename>`
	arn.Resource = "role/" + segments[len(segments)-1]
	return Role(arn.String())
}
