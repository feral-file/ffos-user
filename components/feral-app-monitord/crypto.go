package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"

	"go.uber.org/zap"
)

// EnsureKeyPair checks for the private key and generates a new ED25519 keypair if it's missing.
func EnsureKeyPair() error {
	if _, err := os.Stat(privateKeyFile); err == nil {
		return nil // Key already exists
	}

	logger.Warn("Private key not found. Generating new key pair...")

	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return fmt.Errorf("failed to generate key pair: %w", err)
	}

	if err := savePrivateKey(privateKey, privateKeyFile); err != nil {
		return err
	}
	if err := savePublicKey(publicKey, publicKeyFile); err != nil {
		return err
	}

	logger.Info("Successfully generated and saved key pair.", zap.String("configDir", configDir))
	return nil
}

// savePrivateKey saves the private key to a PEM file.
func savePrivateKey(privateKey ed25519.PrivateKey, path string) error {
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}
	privateKeyPEM := &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes}
	// #nosec G304 -- path is constructed from constants
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create private key file: %w", err)
	}
	defer closeFile(file)
	return pem.Encode(file, privateKeyPEM)
}

// savePublicKey saves the public key to a PEM file.
func savePublicKey(publicKey ed25519.PublicKey, path string) error {
	pkixBytes, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return fmt.Errorf("failed to marshal public key: %w", err)
	}
	publicKeyPEM := &pem.Block{Type: "PUBLIC KEY", Bytes: pkixBytes}
	// #nosec G304 -- path is constructed from constants
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("failed to create public key file: %w", err)
	}
	defer closeFile(file)
	return pem.Encode(file, publicKeyPEM)
}

// SignMessage loads the private key, signs the given data, and returns the signature as a hex string.
func SignMessage(data []byte) (string, error) {
	pemBytes, err := os.ReadFile(privateKeyFile)
	if err != nil {
		return "", fmt.Errorf("failed to read private key file: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("failed to decode PEM block containing private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse private key: %w", err)
	}

	privateKey, ok := key.(ed25519.PrivateKey)
	if !ok {
		return "", fmt.Errorf("key is not an ED25519 private key")
	}

	signature := ed25519.Sign(privateKey, data)
	return hex.EncodeToString(signature), nil
}

// CleanPublicKey reads the public key, removes the PEM headers/footers and newlines.
func CleanPublicKeyBase64() (string, error) {
	data, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return "", fmt.Errorf("invalid PEM block")
	}

	return base64.StdEncoding.EncodeToString(block.Bytes), nil
}
