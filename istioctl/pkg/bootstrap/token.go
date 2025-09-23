// Copyright Solo.io, Inc
//
// Licensed under a Solo commercial license, not Apache License, Version 2 or any other variant

// nolint: gocritic
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/log"
)

func fetchAuthenticationToken(kc kube.CLIClient, name string, namespace string) (string, error) {
	_, terr := kc.Kube().CoreV1().ServiceAccounts(namespace).Create(context.Background(), &v1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	if terr != nil && !kerrors.IsAlreadyExists(terr) {
		return "", terr
	}

	saSecret, err := getOrCreateServiceAccountSecret(name, namespace, kc)
	if err != nil {
		return "", err
	}
	_, token, err := waitForTokenData(kc, saSecret)
	if err != nil {
		return "", err
	}
	return string(token), nil
}

// In Kubernetes 1.24+ we can't assume the secrets will be referenced in the ServiceAccount or be created automatically.
// See https://github.com/istio/istio/issues/38246
func getOrCreateServiceAccountSecret(
	saName, namespace string,
	client kube.CLIClient,
) (*v1.Secret, error) {
	ctx := context.TODO()
	name := saName + "-istio-bootstrap"

	// manually specified secret, make sure it references the ServiceAccount
	secret, err := client.Kube().CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return secret, nil
	}

	// finally, create the sa token secret manually
	// https://kubernetes.io/docs/tasks/configure-pod-container/configure-service-account/#manually-create-a-service-account-api-token
	// TODO ephemeral time-based tokens are preferred; we should re-think this
	log.Infof("Creating token secret for service account %q", saName)
	return client.Kube().CoreV1().Secrets(namespace).Create(ctx, &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{v1.ServiceAccountNameKey: saName},
		},
		Type: v1.SecretTypeServiceAccountToken,
	}, metav1.CreateOptions{})
}

func waitForTokenData(client kube.CLIClient, secret *v1.Secret) (ca, token []byte, err error) {
	ca, token, err = tokenDataFromSecret(secret)
	if err == nil {
		return ca, token, err
	}

	log.Infof("Waiting for data to be populated in %s", secret.Name)
	err = backoff.Retry(
		func() error {
			secret, err = client.Kube().CoreV1().Secrets(secret.Namespace).Get(context.TODO(), secret.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			ca, token, err = tokenDataFromSecret(secret)
			return err
		},
		backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Second), 5))
	return ca, token, err
}

func tokenDataFromSecret(tokenSecret *v1.Secret) (ca, token []byte, err error) {
	var ok bool
	ca, ok = tokenSecret.Data[v1.ServiceAccountRootCAKey]
	if !ok {
		err = errMissingRootCAKey
		return ca, token, err
	}
	token, ok = tokenSecret.Data[v1.ServiceAccountTokenKey]
	if !ok {
		err = errMissingTokenKey
		return ca, token, err
	}
	return ca, token, err
}

var (
	errMissingRootCAKey = fmt.Errorf("no %q data found", v1.ServiceAccountRootCAKey)
	errMissingTokenKey  = fmt.Errorf("no %q data found", v1.ServiceAccountTokenKey)
)
