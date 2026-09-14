// resync.go fetches a fresh order book snapshot over REST, for use after ApplyDelta detects
// a gap in sequence numbers (see OrderBook.MaybeResync).
package orderbook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"kalshi-engine/internal/auth"
	"kalshi-engine/internal/models"
)

// SnapshotFetcher fetches a market's current order book from Kalshi's REST API
// (GET /markets/{ticker}/orderbook), signed the same way as every other Kalshi request.
type SnapshotFetcher struct {
	authClient  *auth.Client
	restBaseURL string // e.g. "https://demo-api.kalshi.co/trade-api/v2" (config.Config.RestBaseURL)
	httpClient  *http.Client
}

// NewSnapshotFetcher creates a SnapshotFetcher against the given REST base URL.
func NewSnapshotFetcher(authClient *auth.Client, restBaseURL string) *SnapshotFetcher {
	return &SnapshotFetcher{
		authClient:  authClient,
		restBaseURL: restBaseURL,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
	}
}

// matches Kalshi's current REST schema for this endpoint
// (docs.kalshi.com/api-reference/market/get-market-orderbook):
//
//	{ "orderbook_fp": { "yes_dollars": [[price, quantity], ...], "no_dollars": [[price, quantity], ...] } }
//
// prices/quantities are fixed-point decimal strings, same as the WebSocket feed - see models.FixedPoint.
type restOrderBookResponse struct {
	OrderbookFP struct {
		YesDollars [][]string `json:"yes_dollars"`
		NoDollars  [][]string `json:"no_dollars"`
	} `json:"orderbook_fp"`
}

// FetchSnapshot fetches the current order book for ticker and returns it as a "snapshot"
// OrderBookEvent ready to hand to OrderBook.ApplySnapshot. Its SeqNum is always 0, since this
// REST endpoint doesn't return one - see the comment on OrderBook.ApplySnapshot for what that
// means for gap detection afterward.
func (f *SnapshotFetcher) FetchSnapshot(ctx context.Context, ticker string) (models.OrderBookEvent, error) {
	reqURL, err := url.Parse(f.restBaseURL)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("invalid REST base URL %q: %w", f.restBaseURL, err)
	}
	reqURL.Path = path.Join(reqURL.Path, "markets", ticker, "orderbook")
	query := reqURL.Query()
	query.Set("depth", "0") // 0 means "all levels" per Kalshi's docs, rather than some default truncated depth
	reqURL.RawQuery = query.Encode()

	// Kalshi's signature covers the timestamp + method + path, WITHOUT the query string
	headers, err := f.authClient.SignRequest(http.MethodGet, reqURL.Path)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("failed to sign resync request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("failed to build resync request: %w", err)
	}
	req.Header = headers

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("resync request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("failed to read resync response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return models.OrderBookEvent{}, fmt.Errorf("resync request returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed restOrderBookResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("failed to parse resync response: %w", err)
	}

	yesLevels, err := parsePairLevels(parsed.OrderbookFP.YesDollars)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("invalid yes levels in resync response: %w", err)
	}
	noLevels, err := parsePairLevels(parsed.OrderbookFP.NoDollars)
	if err != nil {
		return models.OrderBookEvent{}, fmt.Errorf("invalid no levels in resync response: %w", err)
	}

	return models.OrderBookEvent{
		Type:         "snapshot",
		MarketTicker: ticker,
		YesLevels:    yesLevels,
		NoLevels:     noLevels,
		ReceivedAt:   time.Now(),
	}, nil
}

// parsePairLevels converts [[price, quantity], ...] fixed-point string pairs into
// []models.PriceLevel. Same shape/parsing as ingester.parseLevels for the WebSocket feed,
// duplicated here rather than shared since that'd otherwise be the only reason for the two
// packages to import each other.
func parsePairLevels(pairs [][]string) ([]models.PriceLevel, error) {
	levels := make([]models.PriceLevel, 0, len(pairs))
	for _, pair := range pairs {
		if len(pair) != 2 {
			continue
		}
		price, err := models.ParseFixedPoint(pair[0])
		if err != nil {
			return nil, fmt.Errorf("invalid price %q: %w", pair[0], err)
		}
		quantity, err := models.ParseFixedPoint(pair[1])
		if err != nil {
			return nil, fmt.Errorf("invalid quantity %q: %w", pair[1], err)
		}
		levels = append(levels, models.PriceLevel{Price: price, Quantity: quantity})
	}
	return levels, nil
}
