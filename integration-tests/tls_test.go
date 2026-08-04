// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

//go:build integration

package integration

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTheHealthcheckStaysHealthyWithTLSOn is the criterion issue #171 exists
// for, at the interval the deployment actually runs it.
//
// stack.yml runs `/swarmcli-cd healthcheck`, which probes
// http://127.0.0.1:8080 unless the environment says otherwise. Against a TLS
// listener that probe fails, so Swarm marks the task unhealthy and restarts it,
// forever, with the controller working perfectly throughout and nothing in the
// logs saying TLS.
//
// Three probes at stack.yml's own 10s interval, because that file declares
// `retries: 3`: a probe that passed once and then stopped passing would restart
// the task just the same, and would satisfy a single-shot assertion. It drives
// the real binary as a subprocess, so the flags, the listener and the probe are
// all the ones a deployment gets.
func TestTheHealthcheckStaysHealthyWithTLSOn(t *testing.T) {
	dockerClient(t) // skips unless the daemon is a swarm manager
	bin := buildBinary(t)

	const release = "e2e-tls"
	repo := gitRepo(t, chartFiles(release, 1))
	appsFile := filepath.Join(t.TempDir(), "applications.yaml")
	writeFile(t, appsFile, applicationsYAML(release, repo))

	certPath, keyPath := loopbackPair(t)
	const token = "e2e-tls-token"
	addr := freeAddr(t)

	// The URL stack.yml's TLS example exports as SWARMCLI_CD_SERVER, and the one
	// runCLI puts in the environment of every command below. Nothing tells the
	// probe about the certificate: the loopback rule is what carries it.
	server := "https://" + addr

	ctl := startControllerWith(t, bin, t.TempDir(), addr, token,
		"--config", appsFile, "--tls-cert", certPath, "--tls-key", keyPath)
	waitHealthy(t, bin, server, token, ctl)

	// The interval stack.yml declares, not a shortened one: what is being
	// asserted is that the probe keeps answering across the window Swarm judges
	// the task over.
	const interval = 10 * time.Second
	for probe := range 3 {
		if probe > 0 {
			time.Sleep(interval)
		}
		if r := runCLI(t, bin, server, token, "healthcheck"); r.code != 0 {
			t.Fatalf("probe %d: code=%d stderr=%q\n%s", probe, r.code, r.stderr, ctl.log())
		}
	}

	// And the CLI still reaches the same controller, which is the half of the
	// resolution SWARMCLI_CD_CA_CERT exists for: `app` verifies, so without a
	// certificate to verify against it fails on this exact deployment.
	t.Setenv("SWARMCLI_CD_CA_CERT", certPath)
	if r := runCLI(t, bin, server, token, "app", "list"); r.code != 0 || !strings.Contains(r.stdout, release) {
		t.Fatalf("app list over TLS: code=%d stdout=%q stderr=%q\n%s", r.code, r.stdout, r.stderr, ctl.log())
	}
}

// loopbackPair writes the self-signed certificate and key an operator creates
// as the two Docker secrets stack.yml's TLS block names. The loopback SANs are
// what let the CLI verify it once it has been handed the certificate.
func loopbackPair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "swarmcli-cd integration"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	writeFile(t, certPath, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyPath, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	return certPath, keyPath
}
