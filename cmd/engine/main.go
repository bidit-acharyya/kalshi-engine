// Command engine is the entrypoint for the Kalshi trading engine.
// It wires together config, auth, the WebSocket ingester, and the
// per-market order books, then runs until it receives a shutdown signal.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"kalshi-engine/config"
	"kalshi-engine/internal/auth"
	"kalshi-engine/internal/ingester"
	"kalshi-engine/internal/models"
	"kalshi-engine/internal/orderbook"
)

func main() {
	// structured logger, written to stdout as human-readable text
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(logger); err != nil {
		logger.Error("engine exited with error", "error", err)
		os.Exit(1)
	}
}

// run does the actual wiring so main() can stay a thin wrapper around os.Exit.
func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("config loaded", "sandbox", cfg.Sandbox, "tickers", cfg.Tickers)

	authClient, err := auth.NewClient(cfg.APIKeyId, cfg.PrivateKeyPath)
	if err != nil {
		return err
	}

	// one OrderBook per subscribed ticker, looked up by market ticker as events arrive
	books := make(map[string]*orderbook.OrderBook, len(cfg.Tickers))
	for _, ticker := range cfg.Tickers {
		books[ticker] = orderbook.New(ticker, logger)
	}
	// used to fetch a fresh snapshot over REST whenever a book detects a sequence gap
	fetcher := orderbook.NewSnapshotFetcher(authClient, cfg.RestBaseURL)

	// buffered so a slow consumer doesn't immediately block the WebSocket read loop
	eventChannel := make(chan models.OrderBookEvent, 256)
	ing := ingester.New(authClient, cfg.WSURL, cfg.Tickers, eventChannel, logger)

	// cancelled on SIGINT/SIGTERM so both the ingester and the event loop below shut down cleanly
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Run blocks until ctx is cancelled (returns ctx.Err()) or a non-recoverable error occurs
		if err := ing.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Error("ingester stopped unexpectedly", "error", err)
		}
	}()

	// main event loop: pull parsed order book events off the channel, apply each one to the
	// book for its ticker, and kick off a resync if that application revealed a sequence gap
	for {
		select {
		case <-ctx.Done():
			logger.Info("shutdown signal received, waiting for ingester to stop")
			wg.Wait()
			return nil
		case event := <-eventChannel:
			book, ok := books[event.MarketTicker]
			if !ok {
				logger.Warn("received event for untracked ticker", "ticker", event.MarketTicker)
				continue
			}

			var applyErr error
			switch event.Type {
			case "snapshot":
				applyErr = book.ApplySnapshot(event)
			case "delta":
				applyErr = book.ApplyDelta(event)
			default:
				logger.Warn("received event with unknown type", "ticker", event.MarketTicker, "type", event.Type)
				continue
			}
			if applyErr != nil {
				logger.Error("failed to apply order book event", "ticker", event.MarketTicker, "error", applyErr)
				continue
			}

			// cheap no-op unless ApplyDelta just flagged this book as needing a resync
			book.MaybeResync(ctx, fetcher)
		}
	}
}
