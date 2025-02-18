/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Copyright (c) 2021 Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package controller

import (
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/golang/glog"
	"github.com/imdario/mergo"

	apiv1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"

	"github.com/stolostron/management-ingress/pkg/ingress"
	"github.com/stolostron/management-ingress/pkg/ingress/annotations/class"
	"github.com/stolostron/management-ingress/pkg/ingress/annotations/parser"
	"github.com/stolostron/management-ingress/pkg/net/ssl"
)

// syncSecret keeps in sync Secrets used by Ingress rules with the files on
// disk to allow copy of the content of the secret to disk to be used
// by external processes.
func (ic *NGINXController) syncSecret(key string) {
	glog.V(3).Infof("starting syncing of secret %v", key)

	cert, err := ic.getPemCertificate(key)
	if err != nil {
		glog.Warningf("error obtaining PEM from secret %v: %v", key, err)
		return
	}

	// create certificates and add or update the item in the store
	cur, exists := ic.sslCertTracker.Get(key)
	if exists {
		s := cur.(*ingress.SSLCert)
		if s.Equal(cert) {
			// no need to update
			return
		}
		glog.Infof("updating secret %v in the local store", key)
		ic.sslCertTracker.Update(key, cert)
		// this update must trigger an update
		// (like an update event from a change in Ingress)
		ic.syncQueue.Enqueue(&networking.Ingress{})
		return
	}

	glog.Infof("adding secret %v to the local store", key)
	ic.sslCertTracker.Add(key, cert)
	// this update must trigger an update
	// (like an update event from a change in Ingress)
	ic.syncQueue.Enqueue(&networking.Ingress{})
}

// getPemCertificate receives a secret, and creates a ingress.SSLCert as return.
// It parses the secret and verifies if it's a keypair, or a 'ca.crt' secret only.
func (ic *NGINXController) getPemCertificate(secretName string) (*ingress.SSLCert, error) {
	secret, err := ic.listers.Secret.GetByName(secretName)
	if err != nil {
		return nil, fmt.Errorf("error retrieving secret %v: %v", secretName, err)
	}

	cert, okcert := secret.Data[apiv1.TLSCertKey]
	key, okkey := secret.Data[apiv1.TLSPrivateKeyKey]
	ca := secret.Data["ca.crt"]

	// namespace/secretName -> namespace-secretName
	nsSecName := strings.Replace(secretName, "/", "-", -1)

	var s *ingress.SSLCert
	if okcert && okkey {
		if cert == nil {
			return nil, fmt.Errorf("secret %v has no 'tls.crt'", secretName)
		}
		if key == nil {
			return nil, fmt.Errorf("secret %v has no 'tls.key'", secretName)
		}

		// If 'ca.crt' is also present, it will allow this secret to be used in the
		// 'nginx.ingress.kubernetes.io/auth-tls-secret' annotation
		s, err = ssl.AddOrUpdateCertAndKey(nsSecName, cert, key, ca)
		if err != nil {
			return nil, fmt.Errorf("unexpected error creating pem file: %v", err)
		}

		glog.V(3).Infof("found 'tls.crt' and 'tls.key', configuring %v as a TLS Secret (CN: %v)", secretName, s.CN)
		if ca != nil {
			glog.V(3).Infof("found 'ca.crt', secret %v can also be used for Certificate Authentication", secretName)
		}

	} else if ca != nil {
		s, err = ssl.AddCertAuth(nsSecName, ca)

		if err != nil {
			return nil, fmt.Errorf("unexpected error creating pem file: %v", err)
		}

		// makes this secret in 'syncSecret' to be used for Certificate Authentication
		// this does not enable Certificate Authentication
		glog.V(3).Infof("found only 'ca.crt', configuring %v as an Certificate Authentication Secret", secretName)

	} else {
		return nil, fmt.Errorf("no keypair or CA cert could be found in %v", secretName)
	}

	s.Name = secret.Name
	s.Namespace = secret.Namespace
	return s, nil
}

func (ic *NGINXController) checkSSLChainIssues() {
	for _, secretName := range ic.sslCertTracker.ListKeys() {
		s, _ := ic.sslCertTracker.Get(secretName)
		secret := s.(*ingress.SSLCert)

		if secret.FullChainPemFileName != "" {
			// chain already checked
			continue
		}

		data, err := ssl.FullChainCert(secret.PemFileName)
		if err != nil {
			glog.Errorf("unexpected error generating SSL certificate with full intermediate chain CA certs: %v", err)
			continue
		}

		fullChainPemFileName := fmt.Sprintf("%v/%v-%v-full-chain.pem", ingress.DefaultSSLDirectory, secret.Namespace, secret.Name)
		err = ioutil.WriteFile(fullChainPemFileName, data, 0600)
		if err != nil {
			glog.Errorf("unexpected error creating SSL certificate: %v", err)
			continue
		}

		dst := &ingress.SSLCert{}

		err = mergo.MergeWithOverwrite(dst, secret)
		if err != nil {
			glog.Errorf("unexpected error creating SSL certificate: %v", err)
			continue
		}

		dst.FullChainPemFileName = fullChainPemFileName

		glog.Infof("updating local copy of ssl certificate %v with missing intermediate CA certs", secretName)
		ic.sslCertTracker.Update(secretName, dst)
		// this update must trigger an update
		// (like an update event from a change in Ingress)
		ic.syncQueue.Enqueue(&networking.Ingress{})
	}
}

// checkMissingSecrets verify if one or more ingress rules contains a reference
// to a secret that is not present in the local secret store.
// In this case we call syncSecret.
func (ic *NGINXController) checkMissingSecrets() {
	for _, obj := range ic.listers.Ingress.List() {
		ing := obj.(*networking.Ingress)

		if !class.IsValid(ing) {
			continue
		}

		for _, tls := range ing.Spec.TLS {
			if tls.SecretName == "" {
				continue
			}

			key := fmt.Sprintf("%v/%v", ing.Namespace, tls.SecretName)
			if _, ok := ic.sslCertTracker.Get(key); !ok {
				ic.syncSecret(key)
			}
		}

		key, _ := parser.GetStringAnnotation("auth-tls-secret", ing)
		if key == "" {
			continue
		}

		if _, ok := ic.sslCertTracker.Get(key); !ok {
			ic.syncSecret(key)
		}
	}
}
