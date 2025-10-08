package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"
)

const (
	tpmDevice    = "/dev/tpmrm0"
	tpmKeyHandle = 0x81010002
)

// EnsureKeyPair checks for the ECDSA TPM key pair
func EnsureKeyPair() error {
	// Try to open TPM device
	tpm, err := linuxtpm.Open(tpmDevice)
	if err != nil {
		return fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	// Try to read the public key
	cmd := tpm2.ReadPublic{
		ObjectHandle: tpm2.TPMHandle(tpmKeyHandle),
	}
	_, err = cmd.Execute(tpm)
	if err != nil {
		return fmt.Errorf("failed to read public key from TPM: %w", err)
	}

	return nil
}

// SignMessage signs the given data using the TPM-resident private key and returns the signature in hex format.
// Note: data should be the SHA-256 hash of the message to sign.
func SignMessage(data []byte) (string, error) {
	tpm, err := linuxtpm.Open(tpmDevice)
	if err != nil {
		return "", fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	keyHandle := tpm2.TPMHandle(tpmKeyHandle)

	// Use []byte directly as the digest (no tpm2.Digest)
	digest := data // Assumes data is already a SHA-256 hash

	sign := tpm2.Sign{
		KeyHandle: keyHandle,
		Digest:    tpm2.TPM2BDigest{Buffer: digest},
		InScheme: tpm2.TPMTSigScheme{
			Scheme: tpm2.TPMAlgECDSA,
			Details: tpm2.NewTPMUSigScheme(
				tpm2.TPMAlgECDSA,
				&tpm2.TPMSSchemeHash{
					HashAlg: tpm2.TPMAlgSHA256,
				},
			),
		},
		Validation: tpm2.TPMTTKHashCheck{
			Tag: tpm2.TPMSTHashCheck,
		},
	}

	rsp, err := sign.Execute(tpm)
	if err != nil {
		return "", fmt.Errorf("failed to sign with TPM: %w", err)
	}

	// Extract ECDSA signature components
	ecSig, err := rsp.Signature.Signature.ECDSA()
	if err != nil {
		return "", fmt.Errorf("failed to get ECDSA sig : %w", err)
	}
	r := ecSig.SignatureR.Buffer
	s := ecSig.SignatureS.Buffer
	signature := append(r, s...)

	return hex.EncodeToString(signature), nil
}

// CleanPublicKeyBase64 reads the public key and returns it as base64-encoded string.
func CleanPublicKeyBase64() (string, error) {
	tpm, err := linuxtpm.Open(tpmDevice)
	if err != nil {
		return "", fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	cmd := tpm2.ReadPublic{
		ObjectHandle: tpm2.TPMHandle(tpmKeyHandle),
	}
	response, err := cmd.Execute(tpm)
	if err != nil {
		return "", fmt.Errorf("failed to read public key from TPM: %w", err)
	}

	pubBytes := response.OutPublic.Bytes()
	return base64.StdEncoding.EncodeToString(pubBytes), nil
}
