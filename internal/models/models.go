// Package models defines shared types used across all modules in the engine
package models
import "time"

// represents a single update to the order book for a specific market on Kalshi
// ingester produces these, orderbook consumes them
type OrderBookEvent struct {
	Type			string // "snapshot" for full order book, "delta" for incremental changes
	MarketTicker	string // identifies which market this event belongs to (e.g. "KXBTC-25APR16")
	// these arrive in order: 1, 2, 3, 4, ...
	// if you receive 4 and then 6, you know you missed 5 and should wait for the next snapshot before processing more deltas
	SeqNum 			int64 // sequence number assigned by the exchange
	// price levels for the "Yes" and "No" sides of the market
	// for a snapshot, these represent the full order book
	// for a delta, these represent only the price levels that changed since the last event
	YesBids 		[]PriceLevel
	NoBids 			[]PriceLevel
	ReceivedAt 		time.Time // used later to measure tick-to-trade latency (how fast we react to market changes)
}

// represents one row in the order book: the price level and the quantity available at that price
type PriceLevel struct {
	// price of 65 means $0.65, meaning the market is currently assigning a 65% probability to the "Yes" outcome
	Price 		int // in cents (1-99)
	// if 0, it means that price level should be removed from the order book (i.e. no more contracts available at that price)
	Quantity	int // number of contracts available at this price level (1 contract = $1 payout if the outcome is "Yes")
}