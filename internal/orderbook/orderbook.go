// Package orderbook maintains a real-time local replica of the Kalshi exchange order book
package orderbook

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"kalshi-engine/internal/models"
)

// OrderBook maintains a real-time local replica of one market's resting bid orders, for
// both the YES side and the NO side.
//
// Kalshi's order books are bid-only - there's no separate "ask" side, because a NO bid at
// price X is economically identical to a YES ask at price (100 - X), and vice versa - so
// this only ever tracks bids.
type OrderBook struct {
	mutex sync.RWMutex
	ticker string
	// keyed by price in micros (see models.FixedPoint.Micros) -> quantity in micros resting
	// at that price. a price level is removed from the map entirely once its quantity drops
	// to zero or below, rather than being kept around at zero.
	yesBids map[int64]int64
	noBids map[int64]int64
	lastSeq int64 // sequence number of the last successfully applied event; 0 means "no baseline yet"
	needsResync bool // set when we detect a gap in sequence numbers; cleared once a fresh snapshot is applied
	resyncing bool // true while a resync fetch is in flight, so MaybeResync doesn't launch duplicates
	logger *slog.Logger
}

// creates an empty OrderBook for a specified market ticker
func New(ticker string, logger *slog.Logger) *OrderBook {
	return &OrderBook{
		ticker: ticker,
		yesBids: make(map[int64]int64),
		noBids: make(map[int64]int64),
		logger: logger,
	}
}

// ApplySnapshot replaces the entire order book (both sides) with the contents of a
// "snapshot" event and clears any pending resync flag.
func (ob *OrderBook) ApplySnapshot(event models.OrderBookEvent) error {
	if event.Type != "snapshot" {
		return fmt.Errorf("ApplySnapshot called with a %q event, expected \"snapshot\"", event.Type)
	}
	if event.MarketTicker != ob.ticker {
		return fmt.Errorf("event for ticker %q applied to order book for %q", event.MarketTicker, ob.ticker)
	}

	yesBids := make(map[int64]int64, len(event.YesLevels))
	for _, level := range event.YesLevels {
		if level.Quantity.Micros <= 0 {
			continue
		}
		yesBids[level.Price.Micros] = level.Quantity.Micros
	}
	noBids := make(map[int64]int64, len(event.NoLevels))
	for _, level := range event.NoLevels {
		if level.Quantity.Micros <= 0 {
			continue
		}
		noBids[level.Price.Micros] = level.Quantity.Micros
	}

	ob.mutex.Lock()
	defer ob.mutex.Unlock()
	ob.yesBids = yesBids
	ob.noBids = noBids
	// event.SeqNum is 0 for a REST-fetched resync snapshot (that endpoint doesn't return a
	// sequence number), which intentionally resets our baseline: ApplyDelta skips the gap
	// check while lastSeq is 0, so whatever delta arrives next just becomes the new baseline
	// instead of being flagged as another gap.
	ob.lastSeq = event.SeqNum
	ob.needsResync = false
	return nil
}

// ApplyDelta applies a single incremental price-level change from a "delta" event.
//
// If the event's sequence number isn't exactly one more than the last one we applied, the
// book is marked as needing a resync and the delta is NOT applied - our view of that price
// level may already be stale, so guessing would just compound the error. Deltas are also
// skipped (without erroring) while a resync is pending, since applying more changes on top
// of a known-bad book doesn't help. Call MaybeResync after this to actually fetch a fresh
// snapshot when needed.
func (ob *OrderBook) ApplyDelta(event models.OrderBookEvent) error {
	if event.Type != "delta" {
		return fmt.Errorf("ApplyDelta called with a %q event, expected \"delta\"", event.Type)
	}
	if event.MarketTicker != ob.ticker {
		return fmt.Errorf("event for ticker %q applied to order book for %q", event.MarketTicker, ob.ticker)
	}

	ob.mutex.Lock()
	defer ob.mutex.Unlock()

	if ob.lastSeq > 0 && event.SeqNum != ob.lastSeq+1 {
		ob.needsResync = true
		ob.logger.Warn("order book sequence gap detected, marking for resync",
			"ticker", ob.ticker, "expected", ob.lastSeq+1, "received", event.SeqNum)
		return nil
	}
	if ob.needsResync {
		return nil
	}

	var side map[int64]int64
	switch event.Side {
	case "yes":
		side = ob.yesBids
	case "no":
		side = ob.noBids
	default:
		return fmt.Errorf("delta event has unrecognized side %q", event.Side)
	}

	newQuantity := side[event.DeltaPrice.Micros] + event.DeltaQuantity.Micros
	if newQuantity <= 0 {
		delete(side, event.DeltaPrice.Micros)
	} else {
		side[event.DeltaPrice.Micros] = newQuantity
	}
	ob.lastSeq = event.SeqNum
	return nil
}

// NeedsResync reports whether this book detected a sequence gap and is waiting on a fresh
// snapshot before it can be trusted again.
func (ob *OrderBook) NeedsResync() bool {
	ob.mutex.RLock()
	defer ob.mutex.RUnlock()
	return ob.needsResync
}

// BestYesBid returns the highest-priced resting YES bid, or ok == false if there isn't one.
func (ob *OrderBook) BestYesBid() (price, quantity models.FixedPoint, ok bool) {
	ob.mutex.RLock()
	defer ob.mutex.RUnlock()
	return bestOf(ob.yesBids)
}

// BestNoBid returns the highest-priced resting NO bid, or ok == false if there isn't one.
func (ob *OrderBook) BestNoBid() (price, quantity models.FixedPoint, ok bool) {
	ob.mutex.RLock()
	defer ob.mutex.RUnlock()
	return bestOf(ob.noBids)
}

// bestOf finds the highest price key in a side map (bids are always "best" at the highest
// price). Caller must hold at least a read lock.
func bestOf(side map[int64]int64) (price, quantity models.FixedPoint, ok bool) {
	var bestPriceMicros int64
	found := false
	for p := range side {
		if !found || p > bestPriceMicros {
			bestPriceMicros = p
			found = true
		}
	}
	if !found {
		return models.FixedPoint{}, models.FixedPoint{}, false
	}
	return models.FixedPoint{Micros: bestPriceMicros}, models.FixedPoint{Micros: side[bestPriceMicros]}, true
}

// MaybeResync checks whether this book currently needs a resync and, if so and one isn't
// already in flight, kicks one off on a background goroutine using fetcher to pull a fresh
// snapshot over REST. Safe to call after every event - it's a cheap no-op otherwise.
//
// Note: there's an inherent small race here. A delta that arrives between the REST fetch
// completing and ApplySnapshot running is simply dropped (its SeqNum won't line up with the
// fresh lastSeq of 0, so ApplyDelta just treats the next one after it as the new baseline).
// For this project that's an acceptable tradeoff; a fully gap-free implementation would
// buffer WS messages during the resync and replay any with SeqNum after the snapshot.
func (ob *OrderBook) MaybeResync(ctx context.Context, fetcher *SnapshotFetcher) {
	ob.mutex.Lock()
	if !ob.needsResync || ob.resyncing {
		ob.mutex.Unlock()
		return
	}
	ob.resyncing = true
	ob.mutex.Unlock()

	go func() {
		defer func() {
			ob.mutex.Lock()
			ob.resyncing = false
			ob.mutex.Unlock()
		}()

		ob.logger.Info("resyncing order book via REST snapshot", "ticker", ob.ticker)
		snapshot, err := fetcher.FetchSnapshot(ctx, ob.ticker)
		if err != nil {
			ob.logger.Error("resync fetch failed, will retry on the next gap", "ticker", ob.ticker, "error", err)
			return
		}
		if err := ob.ApplySnapshot(snapshot); err != nil {
			ob.logger.Error("failed to apply resync snapshot", "ticker", ob.ticker, "error", err)
			return
		}
		ob.logger.Info("order book resynced successfully", "ticker", ob.ticker)
	}()
}
