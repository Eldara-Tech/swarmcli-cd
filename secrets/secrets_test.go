// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package secrets

import (
	"bytes"
	"context"
	"errors"
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
