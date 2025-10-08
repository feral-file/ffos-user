package main

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
)

const (
	tpmDevice    = "/dev/tpmrm0"
	tpmKeyHandle = 0x81010002
)

// EnsureKeyPair checks for the ECDSA TPM key pair
func EnsureKeyPair() error {
	// Try to open TPM device
	tpm, err := transport.OpenTPM(tpmDevice)
	if err != nil {
		return fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	// Try to read the public key
	_, err = tpm2.ReadPublic(tpm, tpm2.TPMHandle(tpmKeyHandle))
	if err != nil {
		return fmt.Errorf("failed to read public key from TPM: %w", err)
	}

	return nil
}

// SignMessage signs the given data using the TPM-resident private key and returns the signature in hex format.
func SignMessage(data []byte) (string, error) {
	tpm, err := transport.OpenTPM(tpmDevice)
	if err != nil {
		return "", fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	keyHandle := tpm2.TPMHandle(tpmKeyHandle)

	digest := tpm2.TPM2BDigest{
		Buffer: data,
	}

	sign := tpm2.Sign{
		KeyHandle: tpm2.AuthHandle{
			Handle: keyHandle,
			Name:   tpm2.TPM2BName{},
			Auth:   tpm2.PasswordAuth(nil),
		},
		Digest: digest,
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

	ecdsaSig, err := rsp.Signature.Signature.ECDSA()
	if err != nil {
		return "", fmt.Errorf("failed to get ECDSA signature: %w", err)
	}

	r := ecdsaSig.SignatureR.Buffer
	s := ecdsaSig.SignatureS.Buffer
	signature := append(r, s...)

	return hex.EncodeToString(signature), nil
}

// CleanPublicKey reads the public key
func CleanPublicKeyBase64() (string, error) {
	tpm, err := transport.OpenTPM(tpmDevice)
	if err != nil {
		return "", fmt.Errorf("failed to open TPM: %w", err)
	}
	defer tpm.Close()

	pub, _, _, err := tpm2.ReadPublic(tpm, tpm2.TPMHandle(tpmKeyHandle))
	if err != nil {
		return "", fmt.Errorf("failed to read public key from TPM: %w", err)
	}

	pubBytes, err := pub.Encode()

	if err != nil {
		return "", fmt.Errorf("failed to encode public key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(pubBytes), nil
}
