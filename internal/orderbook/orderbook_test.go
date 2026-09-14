package orderbook

import (
	"log/slog"
	"testing"

	"kalshi-engine/internal/models"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

func mustParse(t *testing.T, s string) models.FixedPoint {
	t.Helper()
	fp, err := models.ParseFixedPoint(s)
	if err != nil {
		t.Fatalf("failed to parse fixed point %q: %v", s, err)
	}
	return fp
}

func snapshotEvent(t *testing.T, ticker string, seq int64, yes, no [][2]string) models.OrderBookEvent {
	t.Helper()
	toLevels := func(pairs [][2]string) []models.PriceLevel {
		levels := make([]models.PriceLevel, 0, len(pairs))
		for _, p := range pairs {
			levels = append(levels, models.PriceLevel{Price: mustParse(t, p[0]), Quantity: mustParse(t, p[1])})
		}
		return levels
	}
	return models.OrderBookEvent{
		Type:         "snapshot",
		MarketTicker: ticker,
		SeqNum:       seq,
		YesLevels:    toLevels(yes),
		NoLevels:     toLevels(no),
	}
}

func deltaEvent(t *testing.T, ticker string, seq int64, side, price, delta string) models.OrderBookEvent {
	t.Helper()
	return models.OrderBookEvent{
		Type:          "delta",
		MarketTicker:  ticker,
		SeqNum:        seq,
		Side:          side,
		DeltaPrice:    mustParse(t, price),
		DeltaQuantity: mustParse(t, delta),
	}
}

func TestApplySnapshot(t *testing.T) {
	ob := New("TICK", newTestLogger())
	event := snapshotEvent(t, "TICK", 10,
		[][2]string{{"0.65", "100.00"}, {"0.60", "50.00"}},
		[][2]string{{"0.35", "20.00"}},
	)

	if err := ob.ApplySnapshot(event); err != nil {
		t.Fatalf("ApplySnapshot returned unexpected error: %v", err)
	}

	price, quantity, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected a best YES bid after snapshot")
	}
	if price.Micros != mustParse(t, "0.65").Micros {
		t.Errorf("BestYesBid price = %v, want 0.65", price.Dollars())
	}
	if quantity.Micros != mustParse(t, "100.00").Micros {
		t.Errorf("BestYesBid quantity = %v, want 100", quantity.Dollars())
	}

	if ob.NeedsResync() {
		t.Error("NeedsResync() = true after a clean snapshot, want false")
	}
}

func TestApplySnapshot_WrongTicker(t *testing.T) {
	ob := New("TICK", newTestLogger())
	event := snapshotEvent(t, "OTHER-TICK", 1, nil, nil)
	if err := ob.ApplySnapshot(event); err == nil {
		t.Fatal("expected an error when applying a snapshot for the wrong ticker")
	}
}

func TestApplySnapshot_ZeroQuantityLevelsAreDropped(t *testing.T) {
	ob := New("TICK", newTestLogger())
	event := snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "0.00"}}, nil)
	if err := ob.ApplySnapshot(event); err != nil {
		t.Fatalf("ApplySnapshot returned unexpected error: %v", err)
	}
	if _, _, ok := ob.BestYesBid(); ok {
		t.Error("expected no YES bids after a snapshot containing only a zero-quantity level")
	}
}

func TestApplyDelta_InOrder(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	// seq 2 is exactly lastSeq+1, so this should apply cleanly: 10 + 5 = 15
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 2, "yes", "0.50", "5.00")); err != nil {
		t.Fatalf("ApplyDelta returned unexpected error: %v", err)
	}

	_, quantity, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected a YES bid after delta")
	}
	if want := mustParse(t, "15.00").Micros; quantity.Micros != want {
		t.Errorf("quantity = %d, want %d", quantity.Micros, want)
	}
	if ob.NeedsResync() {
		t.Error("NeedsResync() = true after an in-order delta, want false")
	}
}

func TestApplyDelta_RemovesLevelWhenQuantityHitsZero(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 2, "yes", "0.50", "-10.00")); err != nil {
		t.Fatalf("ApplyDelta returned unexpected error: %v", err)
	}
	if _, _, ok := ob.BestYesBid(); ok {
		t.Error("expected the level to be removed once its quantity reached zero")
	}
}

func TestApplyDelta_GapMarksNeedsResyncAndSkipsTheDelta(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	// skip straight to seq 5 (expected 2): a gap
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 5, "yes", "0.50", "5.00")); err != nil {
		t.Fatalf("ApplyDelta returned unexpected error: %v", err)
	}

	if !ob.NeedsResync() {
		t.Error("expected NeedsResync() = true after a sequence gap")
	}
	// the delta should NOT have been applied - quantity should still be the original 10, not 15
	_, quantity, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected the original YES bid to still be there")
	}
	if want := mustParse(t, "10.00").Micros; quantity.Micros != want {
		t.Errorf("quantity = %d, want %d (delta should have been skipped)", quantity.Micros, want)
	}
}

func TestApplyDelta_SkippedWhileResyncPending(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	// trigger a gap
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 5, "yes", "0.50", "5.00")); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	// a further delta arriving while resync is pending should also be a no-op
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 6, "yes", "0.50", "100.00")); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	_, quantity, _ := ob.BestYesBid()
	if want := mustParse(t, "10.00").Micros; quantity.Micros != want {
		t.Errorf("quantity = %d, want %d (deltas should be ignored while a resync is pending)", quantity.Micros, want)
	}
}

func TestApplySnapshot_ClearsNeedsResync(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 1, [][2]string{{"0.50", "10.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 5, "yes", "0.50", "5.00")); err != nil { // triggers a gap
		t.Fatalf("ApplyDelta: %v", err)
	}
	if !ob.NeedsResync() {
		t.Fatal("expected NeedsResync() = true before the fresh snapshot")
	}

	// a fresh snapshot (as MaybeResync would apply) should clear the flag, regardless of seq
	if err := ob.ApplySnapshot(snapshotEvent(t, "TICK", 0, [][2]string{{"0.55", "20.00"}}, nil)); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	if ob.NeedsResync() {
		t.Error("expected NeedsResync() = false after a fresh snapshot")
	}

	// and a delta right after, even though its seq isn't "1", should now apply cleanly
	// since lastSeq was reset to 0 by the resync snapshot
	if err := ob.ApplyDelta(deltaEvent(t, "TICK", 42, "yes", "0.55", "1.00")); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}
	_, quantity, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected a YES bid")
	}
	if want := mustParse(t, "21.00").Micros; quantity.Micros != want {
		t.Errorf("quantity = %d, want %d", quantity.Micros, want)
	}
}

func TestBestBid_EmptyBook(t *testing.T) {
	ob := New("TICK", newTestLogger())
	if _, _, ok := ob.BestYesBid(); ok {
		t.Error("expected ok = false for an empty book")
	}
	if _, _, ok := ob.BestNoBid(); ok {
		t.Error("expected ok = false for an empty book")
	}
}

func TestBestBid_PicksHighestPrice(t *testing.T) {
	ob := New("TICK", newTestLogger())
	event := snapshotEvent(t, "TICK", 1,
		[][2]string{{"0.40", "1.00"}, {"0.70", "1.00"}, {"0.55", "1.00"}},
		nil,
	)
	if err := ob.ApplySnapshot(event); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}
	price, _, ok := ob.BestYesBid()
	if !ok {
		t.Fatal("expected a best YES bid")
	}
	if want := mustParse(t, "0.70").Micros; price.Micros != want {
		t.Errorf("BestYesBid price = %v, want 0.70", price.Dollars())
	}
}
