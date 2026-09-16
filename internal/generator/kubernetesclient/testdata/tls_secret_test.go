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

func TestServerTLSSecretTrustBundle(t *testing.T) {
	ca, leaf, key := secretCertificate(t, "widget.example.test", time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	otherCA, _, _ := secretCertificate(t, "widget.example.test", time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	block, _ := pem.Decode(leaf)
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range [][]byte{ca, append(append([]byte{}, ca...), otherCA...)} {
		roots, err := ParseServerTLSRoots(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "widget.example.test"}); err != nil {
			t.Fatal(err)
		}
		clear(bundle)
		if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: "widget.example.test"}); err != nil {
			t.Fatal("trust pool retained mutable input", err)
		}
	}
	ca, _, _ = secretCertificate(t, "widget.example.test", time.Now().Add(time.Hour), x509.ExtKeyUsageServerAuth)
	for name, input := range map[string][]byte{
		"empty": nil, "whitespace": []byte(" \n"), "oversize": bytes.Repeat([]byte(" "), (512<<10)+1),
		"too many": bytes.Repeat(ca, 257), "private key": append(append([]byte{}, ca...), key...),
		"leading text": append([]byte("unexpected\n"), ca...), "trailing text": append(append([]byte{}, ca...), []byte("unexpected")...),
		"malformed prefix":    append([]byte("-----BEGIN CERTIFICATE-----\nbroken\n"), ca...),
		"invalid certificate": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid")}),
		"headers":             pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Headers: map[string]string{"Name": "value"}, Bytes: block.Bytes}),
	} {
		t.Run(name, func(t *testing.T) {
			if roots, err := ParseServerTLSRoots(input); !errors.Is(err, ErrResourceObservation) || roots != nil {
				t.Fatal("invalid trust input accepted")
			}
		})
	}
}

func TestServerTLSSecret(t *testing.T) {
	testTLSSecret(t, x509.ExtKeyUsageServerAuth, VerifyServerTLSSecret)
}
func TestClientTLSSecret(t *testing.T) {
	testTLSSecret(t, x509.ExtKeyUsageClientAuth, VerifyClientTLSSecret)
}
func testTLSSecret(t *testing.T, usage x509.ExtKeyUsage, verify func(Object, Owner, TLSSecretTarget) ([]byte, error)) {
	const host = "widget.example.test"
	ca, cert, key := secretCertificate(t, host, time.Now().Add(time.Hour), usage)
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
		got, err := verify(current, owner, target)
		if err != nil || !bytes.Equal(got, cert) {
			t.Fatal("valid certificate rejected", err)
		}
	}
	foreignCA, foreign, foreignKey := secretCertificate(t, host, time.Now().Add(time.Hour), usage)
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
		"noncanonical base64": func(o Object) { o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString(cert) + "\n" },
		"garbage before key": func(o Object) {
			o["data"].(Object)["tls.key"] = base64.StdEncoding.EncodeToString(append([]byte("private-detail\n"), key...))
		},
		"second private key": func(o Object) {
			o["data"].(Object)["tls.key"] = base64.StdEncoding.EncodeToString(append(append([]byte{}, key...), key...))
		},
		"certificate in key": func(o Object) {
			o["data"].(Object)["tls.key"] = base64.StdEncoding.EncodeToString(append(append([]byte{}, key...), cert...))
		},
		"wrong key": func(o Object) { o["data"].(Object)["tls.key"] = base64.StdEncoding.EncodeToString(foreignKey) },
		"private key in certificate": func(o Object) {
			o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString(append(append([]byte(nil), cert...), key...))
		},
		"garbage in certificate": func(o Object) {
			o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString(append([]byte("private-detail\n"), cert...))
		},
		"malformed prefix": func(o Object) {
			o["data"].(Object)["tls.crt"] = base64.StdEncoding.EncodeToString(append([]byte("-----BEGIN CERTIFICATE-----\ninvalid\n"), cert...))
		},
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
			got, err := verify(current, owner, target)
			if !errors.Is(err, ErrResourceObservation) || len(got) != 0 || err.Error() != ErrResourceObservation.Error() {
				t.Fatal("invalid Secret exposed data or passed verification", err)
			}
		})
	}
	for _, bad := range []ServerTLSSecretTarget{{Namespace: target.Namespace, Name: target.Name, DNSName: host}, {Namespace: target.Namespace, Name: target.Name, DNSName: "other.example.test", Roots: roots}, {Namespace: target.Namespace, Name: target.Name, DNSName: "", Roots: roots}} {
		if _, err := verify(fresh(), owner, bad); !errors.Is(err, ErrResourceObservation) {
			t.Fatal("invalid target accepted", err)
		}
	}
	wrongUsage := x509.ExtKeyUsageServerAuth
	if usage == wrongUsage {
		wrongUsage = x509.ExtKeyUsageClientAuth
	}
	for _, tc := range []struct {
		expiry time.Time
		usage  x509.ExtKeyUsage
	}{{time.Now().Add(-time.Minute), usage}, {time.Now().Add(time.Hour), wrongUsage}} {
		ca, crt, key := secretCertificate(t, host, tc.expiry, tc.usage)
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca)
		checked := target
		checked.Roots = pool
		current := fresh()
		current["data"] = Object{"tls.crt": base64.StdEncoding.EncodeToString(crt), "tls.key": base64.StdEncoding.EncodeToString(key)}
		if _, err := verify(current, owner, checked); !errors.Is(err, ErrResourceObservation) {
			t.Fatal("expired certificate or wrong certificate purpose accepted", err)
		}
	}
}
