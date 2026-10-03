package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestVaultKeyFromEnvPrefersNewName(t *testing.T) {
	newKey := bytes.Repeat([]byte{1}, 32)
	legacyKey := bytes.Repeat([]byte{2}, 32)
	t.Setenv("TAMIOPS_VAULT_KEY", base64.StdEncoding.EncodeToString(newKey))
	t.Setenv("TAMI_VAULT_KEY", base64.StdEncoding.EncodeToString(legacyKey))

	got, err := vaultKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newKey) {
		t.Fatal("the legacy key overrode TAMIOPS_VAULT_KEY")
	}
}

func TestVaultKeyFromEnvAcceptsLegacyName(t *testing.T) {
	legacyKey := bytes.Repeat([]byte{3}, 32)
	t.Setenv("TAMIOPS_VAULT_KEY", "")
	t.Setenv("TAMI_VAULT_KEY", base64.StdEncoding.EncodeToString(legacyKey))

	got, err := vaultKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, legacyKey) {
		t.Fatal("legacy environment key was not accepted")
	}
}

func TestVaultKeyFromEnvDoesNotFallBackFromInvalidNewName(t *testing.T) {
	legacyKey := bytes.Repeat([]byte{4}, 32)
	t.Setenv("TAMIOPS_VAULT_KEY", "invalid")
	t.Setenv("TAMI_VAULT_KEY", base64.StdEncoding.EncodeToString(legacyKey))

	if _, err := vaultKeyFromEnv(); err == nil || !strings.Contains(err.Error(), "TAMIOPS_VAULT_KEY") {
		t.Fatalf("invalid preferred key did not fail clearly: %v", err)
	}
}
