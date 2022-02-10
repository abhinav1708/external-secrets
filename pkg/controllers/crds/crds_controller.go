/*
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

package crds

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

const (
	certName               = "tls.crt"
	keyName                = "tls.key"
	caCertName             = "ca.crt"
	caKeyName              = "ca.key"
	rotationCheckFrequency = 12 * time.Hour
	certValidityDuration   = 10 * 365 * 24 * time.Hour
	lookaheadInterval      = 90 * 24 * time.Hour
)

type WebhookType int

const (
	//ValidatingWebhook indicates the webhook is a ValidatingWebhook
	Validating WebhookType = iota
	//MutingWebhook indicates the webhook is a MutatingWebhook
	Mutating
	//CRDConversionWebhook indicates the webhook is a conversion webhook
	CRDConversion
	//APIServiceWebhook indicates the webhook is an extension API server
	APIService
)

type Reconciler struct {
	client.Client
	Log                    logr.Logger
	Scheme                 *runtime.Scheme
	recorder               record.EventRecorder
	SvcLabels              map[string]string
	SecretLabels           map[string]string
	CrdResources           []string
	CertDir                string
	dnsName                string
	CAName                 string
	CAOrganization         string
	RestartOnSecretRefresh bool
}

type WebhookInfo struct {
	//Name is the name of the webhook for a validating or mutating webhook, or the CRD name in case of a CRD conversion webhook
	Name string
	Type WebhookType
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("CustomResourceDefinition", req.NamespacedName)
	if contains(r.CrdResources, req.NamespacedName.Name) {
		err := r.updateCRD(ctx, req)
		if err != nil {
			log.Error(err, "failed to inject conversion webhook")
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) ConvertToWebhookInfo() []WebhookInfo {
	info := make([]WebhookInfo, len(r.CrdResources))
	for p, v := range r.CrdResources {
		r := WebhookInfo{
			Name: v,
			Type: CRDConversion,
		}
		info[p] = r
	}
	return info
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, opts controller.Options) error {
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	res := &unstructured.Unstructured{}
	res.SetGroupVersionKind(crdGVK)
	r.recorder = mgr.GetEventRecorderFor("custom-resource-definition")
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(opts).
		For(res).
		Complete(r)
}

func (r *Reconciler) updateCRD(ctx context.Context, req ctrl.Request) error {
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

	svcList := corev1.ServiceList{}
	err := r.List(context.Background(), &svcList, client.MatchingLabels(r.SvcLabels))
	if err != nil {
		return err
	}
	if len(svcList.Items) != 1 {
		return errors.New("multiple services match labels")
	}
	secretList := corev1.SecretList{}
	err = r.List(context.Background(), &secretList, client.MatchingLabels(r.SecretLabels))
	if err != nil {
		return err
	}
	if len(secretList.Items) != 1 {
		return errors.New("multiple secrets match labels")
	}
	updatedResource := &unstructured.Unstructured{}
	updatedResource.SetGroupVersionKind(crdGVK)
	if err := r.Get(ctx, req.NamespacedName, updatedResource); err != nil {
		return err
	}
	if err := injectSvcToConversionWebhook(updatedResource, &svcList.Items[0]); err != nil {
		return err
	}
	r.dnsName = fmt.Sprintf("%v.%v.svc", svcList.Items[0].Name, svcList.Items[0].Namespace)
	need, err := r.refreshCertIfNeeded(&secretList.Items[0])
	if err != nil {
		return err
	}
	if need {
		artifacts, err := buildArtifactsFromSecret(&secretList.Items[0])
		if err != nil {
			return err
		}
		if err := injectCertToConversionWebhook(updatedResource, artifacts.CertPEM); err != nil {
			return err
		}
	}
	if err := r.Update(ctx, updatedResource); err != nil {
		return err
	}
	return nil
}

func (r *Reconciler) EnsureCertsMounted() bool {
	certFile := r.CertDir + "/" + certName
	_, err := os.Stat(certFile)
	return err == nil
}

func injectSvcToConversionWebhook(crd *unstructured.Unstructured, service *corev1.Service) error {
	_, found, err := unstructured.NestedMap(crd.Object, "spec", "conversion", "webhook", "clientConfig")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("`conversion.webhook.clientConfig` field not found in CustomResourceDefinition")
	}
	if err := unstructured.SetNestedField(crd.Object, service.Name, "spec", "conversion", "webhook", "clientConfig", "service", "name"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(crd.Object, service.Namespace, "spec", "conversion", "webhook", "clientConfig", "service", "namespace"); err != nil {
		return err
	}
	return nil
}

func injectCertToConversionWebhook(crd *unstructured.Unstructured, certPem []byte) error {
	_, found, err := unstructured.NestedMap(crd.Object, "spec", "conversion", "webhook", "clientConfig")
	if err != nil {
		return err
	}
	if !found {
		return errors.New("`conversion.webhook.clientConfig` field not found in CustomResourceDefinition")
	}
	if err := unstructured.SetNestedField(crd.Object, base64.StdEncoding.EncodeToString(certPem), "spec", "conversion", "webhook", "clientConfig", "caBundle"); err != nil {
		return err
	}

	return nil
}

type KeyPairArtifacts struct {
	Cert    *x509.Certificate
	Key     *rsa.PrivateKey
	CertPEM []byte
	KeyPEM  []byte
}

func populateSecret(cert, key []byte, caArtifacts *KeyPairArtifacts, secret *corev1.Secret) {
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[caCertName] = caArtifacts.CertPEM
	secret.Data[caKeyName] = caArtifacts.KeyPEM
	secret.Data[certName] = cert
	secret.Data[keyName] = key
}

func ValidCert(caCert, cert, key []byte, dnsName string, at time.Time) (bool, error) {
	if len(caCert) == 0 || len(cert) == 0 || len(key) == 0 {
		return false, errors.New("empty cert")
	}

	pool := x509.NewCertPool()
	caDer, _ := pem.Decode(caCert)
	if caDer == nil {
		return false, errors.New("bad CA cert")
	}
	cac, err := x509.ParseCertificate(caDer.Bytes)
	if err != nil {
		return false, err
	}
	pool.AddCert(cac)

	_, err = tls.X509KeyPair(cert, key)
	if err != nil {
		return false, err
	}

	b, _ := pem.Decode(cert)
	if b == nil {
		return false, err
	}

	crt, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return false, err
	}
	_, err = crt.Verify(x509.VerifyOptions{
		DNSName:     dnsName,
		Roots:       pool,
		CurrentTime: at,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func lookaheadTime() time.Time {
	return time.Now().Add(lookaheadInterval)
}

func (r *Reconciler) validServerCert(caCert, cert, key []byte) bool {
	valid, err := ValidCert(caCert, cert, key, r.dnsName, lookaheadTime())
	if err != nil {
		return false
	}
	return valid
}

func (r *Reconciler) validCACert(cert, key []byte) bool {
	valid, err := ValidCert(cert, cert, key, r.CAName, lookaheadTime())
	if err != nil {
		return false
	}
	return valid
}

func (r *Reconciler) refreshCertIfNeeded(secret *corev1.Secret) (bool, error) {
	if secret.Data == nil || !r.validCACert(secret.Data[caCertName], secret.Data[caKeyName]) {
		//crLog.Info("refreshing CA and server certs")
		if err := r.refreshCerts(true, secret); err != nil {
			//crLog.Error(err, "could not refresh CA and server certs")
			return false, nil
		}
		//crLog.Info("server certs refreshed")
		if r.RestartOnSecretRefresh {
			//crLog.Info("Secrets have been updated; exiting so pod can be restarted (This behaviour can be changed with the option RestartOnSecretRefresh)")
			os.Exit(0)
		}
		return true, nil
	}
	// make sure our reconciler is initialized on startup (either this or the above refreshCerts() will call this)
	if !r.validServerCert(secret.Data[caCertName], secret.Data[certName], secret.Data[keyName]) {
		//crLog.Info("refreshing server certs")
		if err := r.refreshCerts(false, secret); err != nil {
			//crLog.Error(err, "could not refresh server certs")
			return false, nil
		}
		//crLog.Info("server certs refreshed")
		if r.RestartOnSecretRefresh {
			//crLog.Info("Secrets have been updated; exiting so pod can be restarted (This behaviour can be changed with the option RestartOnSecretRefresh)")
			os.Exit(0)
		}
		return true, nil
	}
	//crLog.Info("no cert refresh needed")
	return true, nil
}

func (r *Reconciler) refreshCerts(refreshCA bool, secret *corev1.Secret) error {
	var caArtifacts *KeyPairArtifacts
	now := time.Now()
	begin := now.Add(-1 * time.Hour)
	end := now.Add(certValidityDuration)
	if refreshCA {
		var err error
		caArtifacts, err = r.CreateCACert(begin, end)
		if err != nil {
			return err
		}
	} else {
		var err error
		caArtifacts, err = buildArtifactsFromSecret(secret)
		if err != nil {
			return err
		}
	}
	cert, key, err := r.CreateCertPEM(caArtifacts, begin, end)
	if err != nil {
		return err
	}
	if err := r.writeSecret(cert, key, caArtifacts, secret); err != nil {
		return err
	}
	return nil
}

func buildArtifactsFromSecret(secret *corev1.Secret) (*KeyPairArtifacts, error) {
	caPem, ok := secret.Data[caCertName]
	if !ok {
		return nil, fmt.Errorf("cert secret is not well-formed, missing %s", caCertName)
	}
	keyPem, ok := secret.Data[caKeyName]
	if !ok {
		return nil, fmt.Errorf("cert secret is not well-formed, missing %s", caKeyName)
	}
	caDer, _ := pem.Decode(caPem)
	if caDer == nil {
		return nil, errors.New("bad CA cert")
	}
	caCert, err := x509.ParseCertificate(caDer.Bytes)
	if err != nil {
		return nil, err
	}
	keyDer, _ := pem.Decode(keyPem)
	if keyDer == nil {
		return nil, err
	}
	key, err := x509.ParsePKCS1PrivateKey(keyDer.Bytes)
	if err != nil {
		return nil, err
	}
	return &KeyPairArtifacts{
		Cert:    caCert,
		CertPEM: caPem,
		KeyPEM:  keyPem,
		Key:     key,
	}, nil
}

func (r *Reconciler) CreateCACert(begin, end time.Time) (*KeyPairArtifacts, error) {
	templ := &x509.Certificate{
		SerialNumber: big.NewInt(0),
		Subject: pkix.Name{
			CommonName:   r.CAName,
			Organization: []string{r.CAOrganization},
		},
		DNSNames: []string{
			r.CAName,
		},
		NotBefore:             begin,
		NotAfter:              end,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, templ, templ, key.Public(), key)
	if err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := pemEncode(der, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}

	return &KeyPairArtifacts{Cert: cert, Key: key, CertPEM: certPEM, KeyPEM: keyPEM}, nil
}

func (r *Reconciler) CreateCertPEM(ca *KeyPairArtifacts, begin, end time.Time) ([]byte, []byte, error) {
	templ := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: r.dnsName,
		},
		DNSNames: []string{
			r.dnsName,
		},
		NotBefore:             begin,
		NotAfter:              end,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.CreateCertificate(rand.Reader, templ, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, nil, err
	}
	certPEM, keyPEM, err := pemEncode(der, key)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

func pemEncode(certificateDER []byte, key *rsa.PrivateKey) ([]byte, []byte, error) {
	certBuf := &bytes.Buffer{}
	if err := pem.Encode(certBuf, &pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}); err != nil {
		return nil, nil, err
	}
	keyBuf := &bytes.Buffer{}
	if err := pem.Encode(keyBuf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		return nil, nil, err
	}
	return certBuf.Bytes(), keyBuf.Bytes(), nil
}

func (r *Reconciler) writeSecret(cert, key []byte, caArtifacts *KeyPairArtifacts, secret *corev1.Secret) error {
	populateSecret(cert, key, caArtifacts, secret)
	return r.Update(context.Background(), secret)
}
