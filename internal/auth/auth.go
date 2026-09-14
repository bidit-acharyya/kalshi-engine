// Package auth handles cryptographic authentication for the Kalshi Exchange API
package auth
import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"
)

// holds credentials needed to authenticate API requests to Kalshi
type Client struct {
	apiKeyId	 string // from Kalshi dashboard
	privateKey	*rsa.PrivateKey // loaded from my .pem file
}

// creates an authenticated client by loading the RSA private key from the specified .pem file
func NewClient(apiKeyId, privateKeyPath string) (*Client, error) {
	// reading the private key from the .pem file
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}
	// pem.Decode strips the -----BEGIN PRIVATE KEY----- and -----END PRIVATE KEY-----
	// block contains .Type (e.g., "PRIVATE KEY") and .Bytes (the actual key data)
	block, _ := pem.Decode(keyBytes)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block containing private key")
	}
	// parse the DER bytes (standardized binary format commonly used to represent cryptographic keys) into an rsa.PrivateKey object
	parsedKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PKCS1 private key: %w", err)
	}
	return &Client{
		apiKeyId: apiKeyId,
		privateKey: parsedKey,
	}, nil
}

// generates the necessary headers to authenticate an API request to Kalshi by creating a signature using the RSA private key
func (c *Client) SignRequest(method, path string) (http.Header, error) {
	timeStampMs := time.Now().UnixMilli()
	timeStampStr := strconv.FormatInt(timeStampMs, 10)
	// the message to be signed is a concatenation of the timestamp, HTTP method, and request path (e.g., "1700000000000GET/api/v1/markets")
	message := timeStampStr + method + path
	hash := sha256.Sum256([]byte(message))
	signature, err := rsa.SignPSS(
		// crypto/rand.Reader is a cryptographically secure random source used for generating the random salt in the PSS signature scheme
		rand.Reader,
		// my RSA private key loaded from the .pem file
		c.privateKey,
		// the hash function we used
		crypto.SHA256,
		hash[:], // [:] converts [32]byte to []byte
		&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to sign message: %w", err)
	}
	// base64 encodes binary -> text using only ASCII characters
	signatureBase64 := base64.StdEncoding.EncodeToString(signature)
	headers := http.Header{}
	headers.Set("KALSHI-ACCESS-KEY", c.apiKeyId)
	headers.Set("KALSHI-ACCESS-TIMESTAMP", timeStampStr)
	headers.Set("KALSHI-ACCESS-SIGNATURE", signatureBase64)
	return headers, nil
}

// generates signed headers specifically for the WebSocket handshake
func (c *Client) WebSocketHeaders() (http.Header, error) {
	return c.SignRequest("GET", "/trade-api/ws/v2")
}