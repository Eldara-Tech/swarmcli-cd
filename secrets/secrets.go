// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

// Package secrets resolves secret material read from an application's source
// tree — a values file committed encrypted, for instance.
//
// It does not supply the controller's own credentials. Git tokens, SSH keys and
// registry authentication arrive as Docker secrets: encrypted at rest in the
// raft log and delivered in memory, which is what swarmcli-rbac-proxy already
// does for its TLS material. Those are configuration, not a seam.
package secrets

import (
	"bytes"
	"context"
	"fmt"

	"github.com/Eldara-Tech/swarmcli-cd/seam"
)

// Request is one piece of material to resolve.
//
// It is a struct rather than a parameter list so that the seam can grow
// without breaking the companion module that implements it: an implementation
// receives the struct, so a field added later — a reference to fetch rather
// than bytes to decrypt, an application scope — costs it nothing. Widening a
// parameter list would be a breaking change to an interface implemented
// outside this repository.
type Request struct {
	// Application names the application whose source tree the material was
	// read from.
	//
	// It is here because a provider holding per-application key material — the
	// shape projects and SOPS take together — cannot otherwise pick a key: a
	// path and some bytes say nothing about who they belong to. It is the
	// application's name as the app set declares it, so it is also the name
	// every log line and API response about that application already uses.
	Application string
	// Path is where the material was read from, relative to the repository
	// root. An implementation may decide by name or extension.
	Path string
	// Data is the material as it was read.
	Data []byte
}

// Provider resolves secret material to plaintext.
//
// A provider that does not recognise the material returns Data unchanged and
// no error, so an unencrypted file passes cleanly through any provider. It
// errors only when it recognises the material and cannot resolve it — a
// corrupt ciphertext, a key it does not hold. Refusing to deploy is the right
// answer there; rendering a stack from a values file that is still ciphertext
// is not.
type Provider interface {
	Resolve(ctx context.Context, req Request) ([]byte, error)
}

var slot seam.Slot[Provider]

// Register installs p as the provider, replacing whatever was there. Call it
// from an init().
func Register(name string, p Provider) { slot.Register(name, p) }

// Get returns the provider in force.
func Get() Provider { return slot.Get() }

// Active names the provider in force, for startup logging.
func Active() string { return slot.Name() }

func init() { Register("plaintext", plaintext{}) }

// plaintext resolves material that needs no resolving, and refuses material it
// can recognise but not decrypt.
//
// Passing everything through is right for a repository whose values files are
// not encrypted, which is every repository until someone encrypts one. It is
// wrong for the one that has: this build has no decryption in it, so returning
// a SOPS file unchanged renders the stack from ciphertext and installs
// ENC[AES256_GCM,data:…] as the literal value of every secret the chart
// declares — no error, no warning, no log line, and a running application whose
// credentials are strings nothing can authenticate with. The interface's own
// contract already says refusing is the right answer there; without this
// nothing could ever reach it.
//
// So the refusal lives here, in the build that cannot decrypt, and needs no
// decryption to make it: recognising an envelope is reading a header.
type plaintext struct{}

// Resolve implements Provider.
func (plaintext) Resolve(_ context.Context, req Request) ([]byte, error) {
	if envelope := recognise(req.Data); envelope != "" {
		return nil, fmt.Errorf("this build cannot decrypt %s material; deploying it would install the "+
			"ciphertext as the value of every secret the chart declares", envelope)
	}
	return req.Data, nil
}

// anchored are the envelopes that occupy a whole file, recognised by what it
// begins with.
//
// Anchoring is the point. A values file may legitimately carry an encrypted
// blob as *data* — a PGP message committed as the value of some key — and a
// provider that scanned for these anywhere in the file would refuse to deploy
// a repository that is not encrypted at all. A file that *begins* with one of
// these is the blob, and is not YAML any engine could render.
var anchored = []struct {
	name   string
	prefix []byte
}{
	{"git-crypt-encrypted", []byte("\x00GITCRYPT\x00")},
	{"ansible-vault-encrypted", []byte("$ANSIBLE_VAULT;")},
	{"age-encrypted", []byte("-----BEGIN AGE ENCRYPTED FILE-----")},
	{"PGP-encrypted", []byte("-----BEGIN PGP MESSAGE-----")},
}

// sopsValue is the marker sops wraps each encrypted value in, and the only
// thing here that is not anchored — sops leaves the document's structure intact
// and puts its metadata wherever the serialiser does, so there is no position to
// anchor to. It is specific enough to carry that: the string names a cipher, a
// data field and sops' own bracket syntax together, and a file containing it
// without being sops output is a file quoting sops output.
//
// It is also the whole of the SOPS test, deliberately. The obvious second
// signal — the sops: metadata block — earns nothing and costs a real file: sops
// encrypts the MAC it writes into that block, so every encrypted document
// carries this marker whatever its format and whatever --encrypted-regex left
// alone, while a values file configuring some sops-aware tool legitimately has a
// top-level sops: key and no ciphertext at all. Refusing that one would abort
// its plan permanently, with no override.
var sopsValue = []byte("ENC[AES256_GCM,data:")

// recognise names the encrypted envelope data is in, or "" if it is not in one
// this build knows about.
//
// Not knowing is the common case and is not an error: a provider that does not
// recognise material returns it unchanged, and every unencrypted values file in
// every repository goes through here.
func recognise(data []byte) string {
	// Leading whitespace only, and not for git-crypt's NUL — trimming space,
	// tabs and newlines leaves a binary header where it was.
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	for _, e := range anchored {
		if bytes.HasPrefix(trimmed, e.prefix) {
			return e.name
		}
	}
	if bytes.Contains(data, sopsValue) {
		return "SOPS-encrypted"
	}
	return ""
}
