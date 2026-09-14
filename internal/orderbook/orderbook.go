// Package orderbook maintains a real-time local replica of the Kalshi exchange order book
package orderbook
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"kalshi-engine/internal/auth"
	"kalshi-engine/internal/models"
)

const maxPrice = 100
type OrderBook struct {
	mutex sync.RWMutex
	ticker string
	// ex: yesBids[65] = 30 means "30 contracts available at $0.65 on the YES side"
	yesBids [maxPrice]int // yesBids[price] = quantity of YES contracts bid at that price
	noBids [maxPrice]int // same applies for NO contracts
	lastSeq int64 // sequence number of the last processed event
	needsResync bool // set to true if we detect gap in sequence numbers
	logger *slog.Logger
}

// creates an OrderBook for a specified market ticker
func New(ticker string, logger *slog.Logger) *OrderBook {
	return &OrderBook{
		ticker: ticker,
		logger: logger,
	}
}

