// Package models defines shared types used across all modules in the engine
package models

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// represents a single update to the order book for a specific market on Kalshi.
// ingester produces these, orderbook consumes them.
//
// Kalshi sends two very different shapes depending on Type:
//   - "snapshot": YesLevels/NoLevels hold the FULL order book for that side (replaces
//     whatever state we had before)
//   - "delta": Side/DeltaPrice/DeltaQuantity describe a single price level changing.
//     DeltaQuantity is a *signed* amount to ADD to whatever quantity is already at
//     DeltaPrice (a negative value removes contracts), NOT an absolute quantity.
type OrderBookEvent struct {
	Type			string // "snapshot" for full order book, "delta" for a single incremental change
	MarketTicker	string // identifies which market this event belongs to (e.g. "KXBTC-25APR16")
	// these arrive in order: 1, 2, 3, 4, ...
	// if you receive 4 and then 6, you know you missed 5 and should wait for the next snapshot before processing more deltas
	SeqNum 			int64 // sequence number assigned by the exchange

	// populated when Type == "snapshot": the full set of price levels for each side
	YesLevels 		[]PriceLevel
	NoLevels 		[]PriceLevel

	// populated when Type == "delta": which side changed, and by how much
	Side			string     // "yes" or "no"
	DeltaPrice		FixedPoint // the price level being changed
	DeltaQuantity	FixedPoint // signed change to apply at DeltaPrice (can be negative)

	ReceivedAt 		time.Time // used later to measure tick-to-trade latency (how fast we react to market changes)
}

// represents one row in the order book: the price level and the quantity available at that price
type PriceLevel struct {
	// dollar price of this level, e.g. 0.65 means the market is currently assigning a 65% probability to the "Yes" outcome
	Price 		FixedPoint
	// number of contracts available at this price level (1 contract = $1 payout if the outcome is "Yes")
	// a quantity of 0 means that price level should be removed from the order book
	Quantity	FixedPoint
}

// fixedPointScale is how many of the underlying int64's units make up 1.0 in real value
// (i.e. 6 decimal digits of precision - far more than Kalshi's API ever sends, so no
// precision is ever lost when parsing).
const fixedPointScale = 1_000_000

// FixedPoint is an exact fixed-point decimal value, stored as an integer scaled up by
// fixedPointScale (so FixedPoint{Micros: 80_000}.Dollars() == 0.08).
//
// Kalshi's "_fp" API fields (yes_dollars_fp, price_dollars, delta_fp, ...) send prices
// and quantities as decimal STRINGS specifically to avoid floating-point rounding, so we
// parse them straight into an integer instead of through float64, and should keep doing
// arithmetic on Micros directly rather than converting to float64 except for display.
type FixedPoint struct {
	Micros int64
}

// ParseFixedPoint parses a Kalshi "_fp"-style decimal string (e.g. "0.0800", "0.960",
// "-54.00") into a FixedPoint, preserving the exact value.
func ParseFixedPoint(s string) (FixedPoint, error) {
	original := s
	s = strings.TrimSpace(s)
	if s == "" {
		return FixedPoint{}, fmt.Errorf("empty fixed-point string")
	}
	negative := false
	switch s[0] {
	case '-':
		negative = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if !hasFrac {
		frac = ""
	}
	if len(frac) > 6 {
		return FixedPoint{}, fmt.Errorf("fixed-point string %q has more than 6 decimal digits, would lose precision", original)
	}
	// right-pad the fractional part with zeros so it's always exactly 6 digits (micros)
	frac += strings.Repeat("0", 6-len(frac))
	wholeVal, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return FixedPoint{}, fmt.Errorf("invalid fixed-point string %q: %w", original, err)
	}
	fracVal, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return FixedPoint{}, fmt.Errorf("invalid fixed-point string %q: %w", original, err)
	}
	micros := wholeVal*fixedPointScale + fracVal
	if negative {
		micros = -micros
	}
	return FixedPoint{Micros: micros}, nil
}

// Dollars returns the value as a float64, for logging/display only.
// Do not use this for further arithmetic - operate on Micros directly to stay exact.
func (f FixedPoint) Dollars() float64 {
	return float64(f.Micros) / fixedPointScale
}
