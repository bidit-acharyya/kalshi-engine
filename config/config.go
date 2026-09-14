// Package config handles loading and validating all configuration for the trading engine
package config
import (
	"fmt"
	"os"
	"strings"
)

// URL Constants
const (
	SandboxRESTBase = "https://demo-api.kalshi.co/trade-api/v2"
	SandboxWSURL = "wss://demo-api.kalshi.co/trade-api/ws/v2"
	ProdRESTBase = "https://api.elections.kalshi.com/trade-api/v2"
	ProdWSURL = "wss://api.elections.kalshi.com/trade-api/ws/v2"
)

// holds every setting the engine needs to run
type Config struct {
	APIKeyId		string
	PrivateKeyPath	string
	// true if using sandbox environment, false if using production environment
	// true means we talk to the demo servers with fake money
	Sandbox			bool
	// tickers you want to subscribe to via WebSocket (e.g. "KXBTC-25APR16", "KXTEMP-25APR16-B50")
	// each ticker represents one specific market on Kalshi that you want to trade on
	Tickers			[]string
	RestBaseURL		string // base URL for REST API calls (depends on whether we're in sandbox or production)
	WSURL			string // WebSocket endpoint for streaming market data (depends on whether we're in sandbox or production)
}

// reads environment variables and returns a fully populated Config object
func Load() (*Config, error) {
	config := &Config{}
	var err error
	config.APIKeyId, err = getRequired("KALSHI_API_KEY_ID")
	if err != nil {
		return nil, err
	}
	config.PrivateKeyPath, err = getRequired("KALSHI_PRIVATE_KEY_PATH")
	if err != nil {
		return nil, err
	}
	// os.Stat returns file info if file exists, and an error if it doesn't
	if _, statErr := os.Stat(config.PrivateKeyPath); os.IsNotExist(statErr) {
		return nil, fmt.Errorf("private key file does not exist at path: %s", config.PrivateKeyPath)
	}
	tickerStr, err := getRequired("KALSHI_TICKERS")
	if err != nil {
		return nil, err
	}
	config.Tickers = strings.Split(tickerStr, ",")
	sandboxStr := os.Getenv("KALSHI_SANDBOX")
	// defaults to true if KALSHI_SANDBOX is not set to prevent accidentally trading with real money
	config.Sandbox = sandboxStr != "false"
	if config.Sandbox {
		config.RestBaseURL = SandboxRESTBase
		config.WSURL = SandboxWSURL
	} else {
		config.RestBaseURL = ProdRESTBase
		config.WSURL = ProdWSURL
	}
	return config, nil
}

// helper function to read an environment variable and return an error if it's not set
// created because os.Getenv returns "" if variable is set to empty string and if variable does not exist, causing confusion
func getRequired(key string) (string, error) {
	value := os.Getenv(key)
	if value == "" {
		return "", fmt.Errorf("missing required environment variable: %s", key)
	}
	return value, nil
}