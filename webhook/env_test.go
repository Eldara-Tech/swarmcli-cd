// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestSecretsFromEnv(t *testing.T) {
	files := map[string]string{
		"/run/secrets/hook":     "whsec_first\n\n  second  \n",
		"/run/secrets/one":      "only",
		"/run/secrets/blank":    " \n\n",
		"/run/secrets/rotating": "old\r\nnew\r\n",
	}
	readFile := func(path string) ([]byte, error) {
		if body, ok := files[path]; ok {
			return []byte(body), nil
		}
		return nil, errors.New("open " + path + ": no such file or directory")
	}

	for _, tc := range []struct {
		name    string
		env     map[string]string
		want    []string
		wantErr string
	}{
		{"neither is not an error", nil, nil, ""},
		{"the variable", map[string]string{EnvSecret: " s3cret \n"}, []string{"s3cret"}, ""},
		{"a file of one", map[string]string{EnvSecretFile: "/run/secrets/one"}, []string{"only"}, ""},
		// Several, so that each GitLab hook's signing token and both halves of a
		// rotation are accepted at once.
		{"a file of several, blank lines skipped", map[string]string{EnvSecretFile: "/run/secrets/hook"}, []string{"whsec_first", "second"}, ""},
		{"CRLF line ends", map[string]string{EnvSecretFile: "/run/secrets/rotating"}, []string{"old", "new"}, ""},
		{"the file wins", map[string]string{EnvSecret: "direct", EnvSecretFile: "/run/secrets/one"}, []string{"only"}, ""},
		// Named rather than read as "no secret", which would silently switch the
		// listener's refusal to start into the wrong message.
		{"an empty file", map[string]string{EnvSecretFile: "/run/secrets/blank"}, nil, "names an empty file"},
		{"an unreadable file", map[string]string{EnvSecretFile: "/run/secrets/absent"}, nil, EnvSecretFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SecretsFromEnv(func(k string) string { return tc.env[k] }, readFile)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("SecretsFromEnv = %v, want an error naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SecretsFromEnv = %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("SecretsFromEnv = %q, want %q", got, tc.want)
			}
		})
	}
}
