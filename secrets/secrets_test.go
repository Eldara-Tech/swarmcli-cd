// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package secrets

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDefaultIsRegistered(t *testing.T) {
	if got := Active(); got != "plaintext" {
		t.Errorf("Active = %q, want plaintext", got)
	}
	if Get() == nil {
		t.Error("Get returned nil")
	}
}

// The default recognises nothing, so everything passes through untouched —
// which is the correct behaviour for a repository whose values files are not
// encrypted.
func TestPlaintextPassesThrough(t *testing.T) {
	want := []byte("replicas: 3\n")

	got, err := Get().Resolve(context.Background(), Request{Path: "values/prod.yaml", Data: want})
	if err != nil {
		t.Fatalf("Resolve = %v, want nil", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Resolve returned %q, want it unchanged", got)
	}
}

func TestPlaintextHandlesNoData(t *testing.T) {
	got, err := Get().Resolve(context.Background(), Request{Path: "values/prod.yaml"})
	if err != nil {
		t.Fatalf("Resolve = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("Resolve returned %q, want empty", got)
	}
}

// The refusal is the whole point of the default having any logic at all: this
// build has no decryption in it, so passing ciphertext through installs
// ENC[AES256_GCM,data:…] as the literal value of every secret the chart
// declares — deployed, running and unauthenticatable, with nothing logged.
func TestPlaintextRefusesEncryptedMaterial(t *testing.T) {
	cases := []struct {
		name     string
		data     string
		envelope string
	}{
		{
			name: "sops metadata block",
			data: "replicas: 3\npassword: ENC[AES256_GCM,data:Qk9,iv:d2,tag:xx,type:str]\n" +
				"sops:\n    mac: ENC[AES256_GCM,data:aa]\n    version: 3.9.0\n",
			envelope: "SOPS",
		},
		{
			// A sops file whose metadata this does not sit at column zero in —
			// JSON, or binary mode — is still recognised by the value marker.
			name:     "sops value marker alone",
			data:     `{"data":"ENC[AES256_GCM,data:Qk9,iv:d2,tag:xx,type:str]"}`,
			envelope: "SOPS",
		},
		{
			// sops encrypts the MAC it writes into its own metadata block, so
			// the value marker is present even in a document whose data keys
			// --encrypted-regex left in the clear.
			name:     "sops metadata whose only ciphertext is the mac",
			data:     "replicas: 3\nsops:\n    mac: ENC[AES256_GCM,data:aa,iv:bb,tag:cc,type:str]\n    version: 3.9.0\n",
			envelope: "SOPS",
		},
		{
			name:     "armored age",
			data:     "-----BEGIN AGE ENCRYPTED FILE-----\nYWJj\n-----END AGE ENCRYPTED FILE-----\n",
			envelope: "age",
		},
		{
			name:     "armored PGP",
			data:     "-----BEGIN PGP MESSAGE-----\n\nhQIMA\n-----END PGP MESSAGE-----\n",
			envelope: "PGP",
		},
		{
			name:     "ansible-vault",
			data:     "$ANSIBLE_VAULT;1.1;AES256\n33623764\n",
			envelope: "ansible-vault",
		},
		{
			name:     "git-crypt",
			data:     "\x00GITCRYPT\x00\x01\x02\x03",
			envelope: "git-crypt",
		},
		{
			// Whatever wrote the file left a blank line first. The envelope did
			// not stop being an envelope.
			name:     "leading whitespace before an armor header",
			data:     "\n  -----BEGIN PGP MESSAGE-----\n\nhQIMA\n",
			envelope: "PGP",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Get().Resolve(context.Background(), Request{
				Application: "web",
				Path:        "values/prod.yaml",
				Data:        []byte(tc.data),
			})
			if err == nil {
				t.Fatalf("Resolve = %q, want a refusal", got)
			}
			if got != nil {
				t.Errorf("Resolve returned %q beside its error; a refusal must return nothing to deploy", got)
			}
			// The operator has to be able to tell which of their files this is
			// about and why nothing can be done about it on this build.
			if !strings.Contains(err.Error(), tc.envelope) {
				t.Errorf("error %q does not name the envelope %q it recognised", err, tc.envelope)
			}
			if !strings.Contains(err.Error(), "cannot decrypt") {
				t.Errorf("error %q does not say this build cannot decrypt it", err)
			}
		})
	}
}

// Anchoring is what keeps the refusal from being worse than the problem. A
// values file may legitimately carry an encrypted blob as data — an operator
// committing a PGP message as the value of some key — and a provider scanning
// for armor headers anywhere would refuse to deploy a repository that is not
// encrypted at all.
func TestPlaintextPassesMaterialThatMerelyContainsAnEnvelope(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{
			name: "a PGP block as the value of a key",
			data: "replicas: 3\nbackupKey: |\n  -----BEGIN PGP MESSAGE-----\n\n  hQIMA\n  -----END PGP MESSAGE-----\n",
		},
		{
			name: "an age file quoted as a value",
			data: "note: \"-----BEGIN AGE ENCRYPTED FILE-----\"\n",
		},
		{
			name: "a nested key spelled sops",
			data: "tooling:\n  sops:\n    enabled: false\n",
		},
		{
			// The one that matters, because it is the file an operator
			// configuring a sops-aware tool actually writes. There is no
			// ciphertext in it anywhere, and refusing it would abort the
			// application's plan every interval with no way to override.
			name: "a top-level sops key with no ciphertext under it",
			data: "image: ghcr.io/acme/api:1.4.2\nreplicas: 3\nsops:\n  enabled: false\n  ageKeyFile: /run/secrets/age.key\n",
		},
		{
			name: "a vault marker in prose",
			data: "description: run $ANSIBLE_VAULT; to encrypt this\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Get().Resolve(context.Background(), Request{Path: "values/prod.yaml", Data: []byte(tc.data)})
			if err != nil {
				t.Fatalf("Resolve = %v, want the material passed through", err)
			}
			if !bytes.Equal(got, []byte(tc.data)) {
				t.Errorf("Resolve returned %q, want it unchanged", got)
			}
		})
	}
}

func TestRegisterReplaces(t *testing.T) {
	original, originalName := Get(), Active()
	t.Cleanup(func() { Register(originalName, original) })

	Register("companion", decrypter{})

	if Active() != "companion" {
		t.Errorf("Active = %q, want companion", Active())
	}

	// Material it recognises is decrypted.
	got, err := Get().Resolve(context.Background(), Request{Path: "values/prod.sops.yaml", Data: []byte("ENC[abc]")})
	if err != nil {
		t.Fatalf("Resolve = %v, want nil", err)
	}
	if string(got) != "abc" {
		t.Errorf("Resolve = %q, want abc", got)
	}

	// Material it does not recognise passes through, with no error.
	plain := []byte("replicas: 3\n")
	got, err = Get().Resolve(context.Background(), Request{Path: "values/prod.yaml", Data: plain})
	if err != nil {
		t.Fatalf("Resolve on unrecognised material = %v, want nil", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("Resolve returned %q, want it unchanged", got)
	}

	// Material it recognises but cannot resolve is an error, not a pass
	// through: rendering a stack from ciphertext is worse than refusing.
	if _, err := Get().Resolve(context.Background(), Request{Path: "values/bad.sops.yaml", Data: []byte("ENC[")}); err == nil {
		t.Error("Resolve on corrupt material = nil, want an error")
	}
}

// decrypter stands in for the Business Edition's SOPS provider, exercising the
// three-way contract the interface documents.
type decrypter struct{}

func (decrypter) Resolve(_ context.Context, req Request) ([]byte, error) {
	if !bytes.HasPrefix(req.Data, []byte("ENC[")) {
		return req.Data, nil
	}
	body, ok := bytes.CutSuffix(bytes.TrimPrefix(req.Data, []byte("ENC[")), []byte("]"))
	if !ok {
		return nil, errors.New("corrupt ciphertext in " + req.Path)
	}
	return body, nil
}
