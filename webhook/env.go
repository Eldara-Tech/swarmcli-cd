// SPDX-License-Identifier: Apache-2.0
// Copyright © 2026 Eldara Tech

package webhook

import (
	"fmt"
	"strings"
)

// Environment variables the secrets are read from. The file form exists for the
// reason the admin token's does: a Docker secret arrives as a file, encrypted at
// rest in the raft log, and the string form gives that up.
const (
	EnvSecret     = "SWARMCLI_CD_WEBHOOK_SECRET"
	EnvSecretFile = "SWARMCLI_CD_WEBHOOK_SECRET_FILE"
)

// SecretsFromEnv reads the secrets a delivery may be signed with, preferring the
// file form. Each non-blank line is one secret: GitLab mints a signing token per
// hook, so a controller several projects push to needs several, and a rotation
// needs the old and the new accepted at once.
//
// None at all is not an error here. Whether it is one depends on whether the
// listener is on, which is the controller's to know.
//
// It takes its environment and file reader as arguments so the tests can drive
// it without touching the process environment.
func SecretsFromEnv(getenv func(string) string, readFile func(string) ([]byte, error)) ([]string, error) {
	text := getenv(EnvSecret)
	path := getenv(EnvSecretFile)
	if path != "" {
		data, err := readFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", EnvSecretFile, err)
		}
		text = string(data)
	}

	var secrets []string
	for line := range strings.Lines(text) {
		if s := strings.TrimSpace(line); s != "" {
			secrets = append(secrets, s)
		}
	}
	if path != "" && len(secrets) == 0 {
		return nil, fmt.Errorf("%s names an empty file", EnvSecretFile)
	}
	return secrets, nil
}
