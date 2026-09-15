package kubernetes

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func secretCertificate(t *testing.T, host string, expiry time.Time, usage x509.ExtKeyUsage) ([]byte, []byte, []byte) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "widget test issuer"}, IsCA: true, BasicConstraintsValid: true, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expiry, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func TestServerTLSSecret(t *testing.T) {
	const host = "widget.example.test"
	ca, cert, key := secretCertificate(t, host, time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	owner := Owner{"test.example/owner": "widget-one"}
	target := ServerTLSSecretTarget{Namespace: "widget", Name: "server-tls", DNSName: host, Roots: roots}
	fresh := func() Object {
		return Object{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls", "metadata": Object{"name": target.Name, "namespace": target.Namespace, "uid": "secret-one", "resourceVersion": "4", "labels": Object{"test.example/owner": "widget-one"}}, "data": Object{"tls.crt": base64.StdEncoding.EncodeToString(cert), "tls.key": base64.StdEncoding.EncodeToString(key)}}
	}
	for _, withUntrustedCA := range []bool{false, true} {
		current := fresh()
		if withUntrustedCA {
			current["data"].(Object)["ca.crt"] = "not a trust anchor"
		}
		got, err := VerifyServerTLSSecret(current, owner, target)
		if err != nil || !bytes.Equal(got, cert) {
			t.Fatal("valid certificate rejected", err)
		}
	}
	foreignCA, foreign, foreignKey := secretCertificate(t, host, time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	cases := map[string]func(Object){
		"owner":               func(o Object) { o["metadata"].(Object)["labels"] = Object{"test.example/owner": "other"} },
		"name":                func(o Object) { o["metadata"].(Object)["name"] = "other" },
		"namespace":           func(o Object) { o["metadata"].(Object)["namespace"] = "other" },
		"uid":                 func(o Object) { delete(o["metadata"].(Object), "uid") },
		"version":             func(o Object) { delete(o["metadata"].(Object), "resourceVersion") },
		"deleting":            func(o Object) { o["metadata"].(Object)["deletionTimestamp"] = "2026-09-15T00:00:00Z" },
		"invalid deletion":    func(o Object) { o["metadata"].(Object)["deletionTimestamp"] = false },
		"kind":                func(o Object) { o["kind"] = "ConfigMap" },
		"api":                 func(o Object) { o["apiVersion"] = "example.test/v1" },
		"type":                func(o Object) { o["type"] = "Opaque" },
		"missing certificate": func(o Object) { delete(o["data"].(Object), "tls.crt") },
		"invalid base64":      func(o Object) { o["data"].(Object)["tls.crt"] = "private-detail" },
		"invalid PEM": func(o Object) {
			o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString([]byte("private-detail"))
		},
		"large certificate": func(o Object) {
			o["data"].(Object)["tls.crt"] = strings.Repeat("A", base64.StdEncoding.EncodedLen(512<<10)+1)
		},
		"large key": func(o Object) {
			o["data"].(Object)["tls.key"] = strings.Repeat("A", base64.StdEncoding.EncodedLen(64<<10)+1)
		},
		"wrong key": func(o Object) { o["data"].(Object)["tls.key"] = base64.StdEncoding.EncodeToString(foreignKey) },
		"self supplied CA": func(o Object) {
			o["data"] = Object{"tls.crt": base64.StdEncoding.EncodeToString(foreign), "tls.key": base64.StdEncoding.EncodeToString(foreignKey), "ca.crt": base64.StdEncoding.EncodeToString(foreignCA)}
		},
		"long chain": func(o Object) {
			o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString(append(append([]byte(nil), cert...), bytes.Repeat(ca, 16)...))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			current := fresh()
			change(current)
			got, err := VerifyServerTLSSecret(current, owner, target)
			if !errors.Is(err, ErrResourceObservation) || len(got) != 0 || err.Error() != ErrResourceObservation.Error() {
				t.Fatal("invalid Secret exposed data or passed verification", err)
			}
		})
	}
	for _, bad := range []ServerTLSSecretTarget{{Namespace: target.Namespace, Name: target.Name, DNSName: host}, {Namespace: target.Namespace, Name: target.Name, DNSName: "other.example.test", Roots: roots}, {Namespace: target.Namespace, Name: target.Name, DNSName: "", Roots: roots}} {
		if _, err := VerifyServerTLSSecret(fresh(), owner, bad); !errors.Is(err, ErrResourceObservation) {
			t.Fatal("invalid target accepted", err)
		}
	}
	for _, tc := range []struct {
		expiry time.Time
		usage  x509.ExtKeyUsage
	}{{time.Now().Add(-time.Minute), x509.ExtKeyUsageServerAuth}, {time.Now().Add(time.Hour), x509.ExtKeyUsageClientAuth}} {
		ca, crt, key := secretCertificate(t, host, tc.expiry, tc.usage)
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		checked := target
		checked.Roots = pool
		current := fresh()
		current["data"] = Object{"tls.crt": base64.StdEncoding.EncodeToString(crt), "tls.key": base64.StdEncoding.EncodeToString(key)}
		if _, err := VerifyServerTLSSecret(current, owner, checked); !errors.Is(err, ErrResourceObservation) {
			t.Fatal("expired or client-only certificate accepted", err)
		}
	}
}
