// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"crypto/sha256"
	"errors"
	"net/url"
	"strings"
)

func CanonicalOrigin(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty origin")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if scheme == "" || host == "" {
		return "", errors.New("missing scheme or host")
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else if scheme == "http" {
			port = "80"
		}
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

func FailureDomainID(origin string) [32]byte {
	canonical, err := CanonicalOrigin(origin)
	if err != nil {
		canonical = strings.ToLower(strings.TrimSpace(origin))
	}
	return sha256.Sum256([]byte(canonical))
}
