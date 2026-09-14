package ingester

import (
	"log/slog"
	"strings"
	"testing"

	"kalshi-engine/internal/models"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newTestIngester(logOutput *strings.Builder) (*Ingester, chan models.OrderBookEvent) {
	var handler slog.Handler
	if logOutput != nil {
		handler = slog.NewTextHandler(logOutput, nil)
	} else {
		handler = slog.NewTextHandler(discardWriter{}, nil)
	}
	logger := slog.New(handler)
	eventChannel := make(chan models.OrderBookEvent, 10)
	ing := New(nil, "wss://example.invalid", []string{"TEST-TICKER"}, eventChannel, logger)
	return ing, eventChannel
}

func TestHandleMessage_Snapshot(t *testing.T) {
	ing, events := newTestIngester(nil)

	raw := []byte(`{
		"type": "orderbook_snapshot",
		"seq": 5,
		"msg": {
			"market_ticker": "TEST-TICKER",
			"yes_dollars_fp": [["0.0800", "300.00"], ["0.2200", "10.00"]],
			"no_dollars_fp": [["0.5400", "20.00"]]
		}
	}`)
	ing.handleMessage(raw)

	select {
	case event := <-events:
		if event.Type != "snapshot" {
			t.Errorf("Type = %q, want %q", event.Type, "snapshot")
		}
		if event.MarketTicker != "TEST-TICKER" {
			t.Errorf("MarketTicker = %q, want %q", event.MarketTicker, "TEST-TICKER")
		}
		if event.SeqNum != 5 {
			t.Errorf("SeqNum = %d, want 5", event.SeqNum)
		}
		if len(event.YesLevels) != 2 {
			t.Fatalf("len(YesLevels) = %d, want 2", len(event.YesLevels))
		}
		if event.YesLevels[0].Price.Micros != 80_000 || event.YesLevels[0].Quantity.Micros != 300_000_000 {
			t.Errorf("YesLevels[0] = %+v, want price=80000 quantity=300000000", event.YesLevels[0])
		}
		if len(event.NoLevels) != 1 {
			t.Fatalf("len(NoLevels) = %d, want 1", len(event.NoLevels))
		}
	default:
		t.Fatal("expected an event to be forwarded, got none")
	}
}

func TestHandleMessage_Delta(t *testing.T) {
	ing, events := newTestIngester(nil)

	raw := []byte(`{
		"type": "orderbook_delta",
		"seq": 6,
		"msg": {
			"market_ticker": "TEST-TICKER",
			"price_dollars": "0.960",
			"delta_fp": "-54.00",
			"side": "yes"
		}
	}`)
	ing.handleMessage(raw)

	select {
	case event := <-events:
		if event.Type != "delta" {
			t.Errorf("Type = %q, want %q", event.Type, "delta")
		}
		if event.Side != "yes" {
			t.Errorf("Side = %q, want %q", event.Side, "yes")
		}
		if event.DeltaPrice.Micros != 960_000 {
			t.Errorf("DeltaPrice.Micros = %d, want 960000", event.DeltaPrice.Micros)
		}
		if event.DeltaQuantity.Micros != -54_000_000 {
			t.Errorf("DeltaQuantity.Micros = %d, want -54000000", event.DeltaQuantity.Micros)
		}
	default:
		t.Fatal("expected an event to be forwarded, got none")
	}
}

func TestHandleMessage_UnknownType(t *testing.T) {
	ing, events := newTestIngester(nil)
	ing.handleMessage([]byte(`{"type": "some_other_message", "seq": 1, "msg": {}}`))
	select {
	case event := <-events:
		t.Fatalf("expected no event for an unrecognized message type, got %+v", event)
	default:
	}
}

func TestHandleMessage_MalformedEnvelope(t *testing.T) {
	ing, events := newTestIngester(nil)
	ing.handleMessage([]byte(`not valid json at all`))
	select {
	case event := <-events:
		t.Fatalf("expected no event for malformed JSON, got %+v", event)
	default:
	}
}

func TestHandleDelta_InvalidPrice(t *testing.T) {
	ing, events := newTestIngester(nil)
	raw := []byte(`{
		"type": "orderbook_delta",
		"seq": 1,
		"msg": {"market_ticker": "TEST-TICKER", "price_dollars": "not-a-number", "delta_fp": "1.00", "side": "yes"}
	}`)
	ing.handleMessage(raw)
	select {
	case event := <-events:
		t.Fatalf("expected no event when price fails to parse, got %+v", event)
	default:
	}
}

func TestCheckSeqGap_DetectsGapAndLogsWarning(t *testing.T) {
	var logOutput strings.Builder
	ing, _ := newTestIngester(&logOutput)

	ing.checkSeqGap("TEST-TICKER", 1, "delta")
	ing.checkSeqGap("TEST-TICKER", 2, "delta")
	// jump from 2 straight to 5: a gap of 2 missed sequence numbers (3 and 4)
	ing.checkSeqGap("TEST-TICKER", 5, "delta")

	if got := ing.seqNums["TEST-TICKER"]; got != 5 {
		t.Errorf("seqNums[TEST-TICKER] = %d, want 5 (should always track the latest, even after a gap)", got)
	}
	if !strings.Contains(logOutput.String(), "sequence number gap detected") {
		t.Errorf("expected a gap warning in the log output, got: %s", logOutput.String())
	}
}

func TestCheckSeqGap_NoGapNoWarning(t *testing.T) {
	var logOutput strings.Builder
	ing, _ := newTestIngester(&logOutput)

	ing.checkSeqGap("TEST-TICKER", 1, "delta")
	ing.checkSeqGap("TEST-TICKER", 2, "delta")
	ing.checkSeqGap("TEST-TICKER", 3, "delta")

	if strings.Contains(logOutput.String(), "sequence number gap detected") {
		t.Errorf("did not expect a gap warning, got: %s", logOutput.String())
	}
}

func TestParseLevels(t *testing.T) {
	levels, err := parseLevels([][]string{{"0.5000", "10.00"}, {"0.6500", "5.00"}})
	if err != nil {
		t.Fatalf("parseLevels returned unexpected error: %v", err)
	}
	if len(levels) != 2 {
		t.Fatalf("len(levels) = %d, want 2", len(levels))
	}
	if levels[0].Price.Micros != 500_000 || levels[0].Quantity.Micros != 10_000_000 {
		t.Errorf("levels[0] = %+v, want price=500000 quantity=10000000", levels[0])
	}
}

func TestParseLevels_SkipsMalformedPairs(t *testing.T) {
	levels, err := parseLevels([][]string{{"0.50"}, {"0.50", "1.00", "extra"}, {"0.65", "2.00"}})
	if err != nil {
		t.Fatalf("parseLevels returned unexpected error: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("len(levels) = %d, want 1 (malformed pairs should be skipped)", len(levels))
	}
}

func TestParseLevels_InvalidPriceIsAnError(t *testing.T) {
	_, err := parseLevels([][]string{{"not-a-price", "1.00"}})
	if err == nil {
		t.Fatal("expected an error for an unparseable price")
	}
}
