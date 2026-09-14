package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeTestKey generates a throwaway RSA key, PEM-encodes it as PKCS1, and writes it to a
// temp file, returning both the file path and the key (so tests can verify signatures against it).
func writeTestKey(t *testing.T) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate test RSA key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	path := filepath.Join(t.TempDir(), "test_key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("failed to write test key file: %v", err)
	}
	return path, key
}

func TestNewClient(t *testing.T) {
	path, _ := writeTestKey(t)

	client, err := NewClient("test-key-id", path)
	if err != nil {
		t.Fatalf("NewClient returned unexpected error: %v", err)
	}
	if client.apiKeyId != "test-key-id" {
		t.Errorf("apiKeyId = %q, want %q", client.apiKeyId, "test-key-id")
	}
}

func TestNewClient_MissingFile(t *testing.T) {
	if _, err := NewClient("test-key-id", "/does/not/exist.pem"); err == nil {
		t.Fatal("NewClient with a missing key file should return an error")
	}
}

func TestNewClient_InvalidPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(path, []byte("not a real pem file"), 0o600); err != nil {
		t.Fatalf("failed to write bad key file: %v", err)
	}
	if _, err := NewClient("test-key-id", path); err == nil {
		t.Fatal("NewClient with an invalid PEM file should return an error")
	}
}

func TestSignRequest(t *testing.T) {
	path, key := writeTestKey(t)
	client, err := NewClient("test-key-id", path)
	if err != nil {
		t.Fatalf("NewClient returned unexpected error: %v", err)
	}

	before := time.Now().UnixMilli()
	headers, err := client.SignRequest("GET", "/trade-api/v2/markets/FOO/orderbook")
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatalf("SignRequest returned unexpected error: %v", err)
	}

	if got := headers.Get("KALSHI-ACCESS-KEY"); got != "test-key-id" {
		t.Errorf("KALSHI-ACCESS-KEY = %q, want %q", got, "test-key-id")
	}

	tsStr := headers.Get("KALSHI-ACCESS-TIMESTAMP")
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		t.Fatalf("KALSHI-ACCESS-TIMESTAMP %q is not an integer: %v", tsStr, err)
	}
	if ts < before || ts > after {
		t.Errorf("KALSHI-ACCESS-TIMESTAMP %d is not within [%d, %d]", ts, before, after)
	}

	sigStr := headers.Get("KALSHI-ACCESS-SIGNATURE")
	sig, err := base64.StdEncoding.DecodeString(sigStr)
	if err != nil {
		t.Fatalf("KALSHI-ACCESS-SIGNATURE is not valid base64: %v", err)
	}

	// verify the signature actually corresponds to the message Kalshi expects:
	// timestamp + method + path, hashed with SHA256 and PSS-verified against the public key
	message := tsStr + "GET" + "/trade-api/v2/markets/FOO/orderbook"
	hash := sha256.Sum256([]byte(message))
	if err := rsa.VerifyPSS(&key.PublicKey, crypto.SHA256, hash[:], sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}); err != nil {
		t.Errorf("signature does not verify against the expected message: %v", err)
	}
}

func TestWebSocketHeaders(t *testing.T) {
	path, _ := writeTestKey(t)
	client, err := NewClient("test-key-id", path)
	if err != nil {
		t.Fatalf("NewClient returned unexpected error: %v", err)
	}

	headers, err := client.WebSocketHeaders()
	if err != nil {
		t.Fatalf("WebSocketHeaders returned unexpected error: %v", err)
	}
	if headers.Get("KALSHI-ACCESS-SIGNATURE") == "" {
		t.Error("WebSocketHeaders did not set KALSHI-ACCESS-SIGNATURE")
	}
}
