package orderbook

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kalshi-engine/internal/auth"
)

func newTestAuthClient(t *testing.T) *auth.Client {
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
	client, err := auth.NewClient("test-key-id", path)
	if err != nil {
		t.Fatalf("auth.NewClient returned unexpected error: %v", err)
	}
	return client
}

func TestFetchSnapshot(t *testing.T) {
	var gotPath, gotQuery, gotSignatureHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotSignatureHeader = r.Header.Get("KALSHI-ACCESS-SIGNATURE")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"orderbook_fp": {
				"yes_dollars": [["0.6500", "100.00"]],
				"no_dollars": [["0.3500", "20.00"]]
			}
		}`))
	}))
	defer server.Close()

	authClient := newTestAuthClient(t)
	fetcher := NewSnapshotFetcher(authClient, server.URL+"/trade-api/v2")

	event, err := fetcher.FetchSnapshot(context.Background(), "TEST-TICKER")
	if err != nil {
		t.Fatalf("FetchSnapshot returned unexpected error: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/trade-api/v2/markets/TEST-TICKER/orderbook") {
		t.Errorf("request path = %q, want to end with /trade-api/v2/markets/TEST-TICKER/orderbook", gotPath)
	}
	if gotQuery != "depth=0" {
		t.Errorf("request query = %q, want %q", gotQuery, "depth=0")
	}
	if gotSignatureHeader == "" {
		t.Error("expected a KALSHI-ACCESS-SIGNATURE header on the resync request")
	}

	if event.Type != "snapshot" {
		t.Errorf("Type = %q, want %q", event.Type, "snapshot")
	}
	if event.MarketTicker != "TEST-TICKER" {
		t.Errorf("MarketTicker = %q, want %q", event.MarketTicker, "TEST-TICKER")
	}
	if event.SeqNum != 0 {
		t.Errorf("SeqNum = %d, want 0 (REST snapshots don't carry a sequence number)", event.SeqNum)
	}
	if len(event.YesLevels) != 1 || event.YesLevels[0].Price.Micros != 650_000 {
		t.Errorf("YesLevels = %+v, want one level at price 650000", event.YesLevels)
	}
	if len(event.NoLevels) != 1 || event.NoLevels[0].Price.Micros != 350_000 {
		t.Errorf("NoLevels = %+v, want one level at price 350000", event.NoLevels)
	}
}

func TestFetchSnapshot_NonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "bad signature"}`))
	}))
	defer server.Close()

	authClient := newTestAuthClient(t)
	fetcher := NewSnapshotFetcher(authClient, server.URL+"/trade-api/v2")

	if _, err := fetcher.FetchSnapshot(context.Background(), "TEST-TICKER"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

func TestMaybeResync_AppliesFreshSnapshotAndClearsFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"orderbook_fp": {
				"yes_dollars": [["0.70", "50.00"]],
				"no_dollars": []
			}
		}`))
	}))
	defer server.Close()

	authClient := newTestAuthClient(t)
	fetcher := NewSnapshotFetcher(authClient, server.URL+"/trade-api/v2")

	ob := New("TEST-TICKER", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TEST-TICKER", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	// trigger a gap so needsResync is set
	if err := ob.ApplyDelta(deltaEvent(t, "TEST-TICKER", 5, "yes", "0.50", "1.00")); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !ob.NeedsResync() {
		t.Fatal("expected NeedsResync() = true before calling MaybeResync")
	}

	ob.MaybeResync(context.Background(), fetcher)

	// MaybeResync fetches on a background goroutine, so poll briefly for it to finish
	deadline := time.Now().Add(2 * time.Second)
	for ob.NeedsResync() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if ob.NeedsResync() {
		t.Fatal("expected NeedsResync() = false after MaybeResync completed")
	}
	price, quantity, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected a YES bid from the resynced snapshot")
	}
	if want := mustParse(t, "0.70").Micros; price.Micros != want {
		t.Errorf("price = %v, want 0.70", price.Dollars())
	}
	if want := mustParse(t, "50.00").Micros; quantity.Micros != want {
		t.Errorf("quantity = %v, want 50", quantity.Dollars())
	}
}
