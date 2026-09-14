// Package ingester manages real-time WebSocket connection to Kalshi
package ingester
import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
	"github.com/gorilla/websocket"
	"kalshi-engine/internal/auth"
	"kalshi-engine/internal/models"
)

// manages the WebSocket lifecycle and event parsing
type Ingester struct {
	authClient		*auth.Client // signs WebSocket hanshake headers
	wsURL			string       // WebSocket endpoint URL
	tickers			[]string     // list of market tickers to subscribe to
	eventChannel	chan<- models.OrderBookEvent // channel to send parsed order book events to
	logger			*slog.Logger   // for structured logging
	// WebSocket connections are NOT thread-safe
	mutex			sync.Mutex     // protects the conn so we don't read/write to the WebSocket concurrently
	conn			*websocket.Conn // the WebSocket connection to Kalshi
	seqNums			map[string]int64 // tracks the latest sequence number we've seen for each ticker to ensure we process events in order and don't miss any
}

// creates a new Ingester but does NOT connect yet (call Run() to connect)
func New(authClient *auth.Client, wsURL string, tickers []string, eventChannel chan<- models.OrderBookEvent, logger *slog.Logger) *Ingester {
	return &Ingester{
		authClient: authClient,
		wsURL: wsURL,
		tickers: tickers,
		eventChannel: eventChannel,
		logger: logger,
		seqNums: make(map[string]int64),
	}
}

/*** ----------WebSocket JSON Message Types---------- ***/

// outer envelope for every message from Kalshi
type wsMessage struct {
	ID		int `json:"id"`
	Type	string `json:"type"` // "orderbook_snapshot" or "orderbook_delta"
	// we parse this separately based on the Type field
	// this avoids wasting CPU time parsing a field we might not understand
	Msg		json.RawMessage `json:"msg"` // the actual data payload
	Seq		int64 `json:"seq"` // sequence number
	Sid		string `json:"sid"` // subscription ID (e.g. "KXBTC-25APR16-12345")
}

// inner payload for both snapshots and deltas
type wsOrderBookData struct {
	MarketTicker	string `json:"market_ticker"`
	YesBids			[][]int `json:"yes"` // array of [price, quantity] pairs
	NoBids			[][]int `json:"no"` // array of [price, quantity] pairs
}

// JSON command we send TO Kalshi to subscribe to a market's order book updates
type subscribeCommand struct {
	ID		int `json:"id"`
	Command	string `json:"cmd"` // "subscribe"
	Params	subscribeParams `json:"params"`
}

type subscribeParams struct {
	Channels 		[]string `json:"channels"` // e.g. ["orderbook.KXBTC-25APR16"]
	MarketTickers	[]string `json:"market_tickers"`
}

// starts the ingester, it connects, subscribes, and reads messages in a loop until the context is cancelled (e.g. on shutdown) or an error occurs
func (ing *Ingester) Run(ctx context.Context) error {
	// connect (and reconnect) on failure
	for {
		select {
		case <-ctx.Done(): // context cancellation means we should shut down
			ing.logger.Info("ingester received shutdown signal, closing WebSocket connection")
			return ctx.Err()
		default: // try to connect and run
			err := ing.connectAndStream(ctx) // only returns an error if connection or subscription fails, otherwise it runs until context cancellation
			if err != nil {
				ing.logger.Error("WebSocket connection lost", "error", err)
			}
			// if we get here, it means the connection was lost, so we wait a bit and then try to reconnect
			// EXPONENTIAL BACKOFF would be better here, but for simplicity we just wait a fixed 3 seconds before reconnecting
			ing.logger.Info("attempting to reconnect in 3 seconds...")
			select {
			case <-ctx.Done(): // case A: someone cancelled the context (shutdown signal) while we were waiting to reconnect
				return ctx.Err()
			case <-time.After(3 * time.Second): // case B: the 3 second timer finished and we should try to reconnect now
				// loop will restart and attempt to connect again
			}
		}
	}
}

// handles one WebSocket session: connect -> subscribe -> read loop
func (ing *Ingester) connectAndStream(ctx context.Context) error {
	// get signed headers for the WebSocket handshake
	headers, err := ing.authClient.WebSocketHeaders()
	if err != nil {
		return fmt.Errorf("failed to sign WebSocket headers: %w", err)
	}
	ing.logger.Info("connecting to WebSocket", "url", ing.wsURL)
	// establish the WebSocket connection to Kalshi
	conn, response, err := websocket.DefaultDialer.DialContext(ctx, ing.wsURL, http.Header(headers))
	if err != nil {
		if response != nil {
			return fmt.Errorf("WebSocket handshake failed (HTTP %d): %w", response.StatusCode, err)
		}
		return fmt.Errorf("failed to connect to WebSocket: %w", err)
	}
	ing.mutex.Lock()
	ing.conn = conn
	ing.mutex.Unlock()
	// ensure the connection is closed when this function exits
	defer func() {
		conn.Close()
		ing.mutex.Lock()
		ing.conn = nil
		ing.mutex.Unlock()
	}()
	ing.logger.Info("successfully connected to WebSocket, subscribing to channels")
	// subscribe to the orderbook_delta channel
	sub := subscribeCommand{
		ID: 1,
		Command: "subscribe",
		Params: subscribeParams{
			Channels: []string{"orderbook_delta"},
			MarketTickers: ing.tickers,
		},
	}
	if err := conn.WriteJSON(sub); err != nil {
		return fmt.Errorf("failed to send subscribe command: %w", err)
	}
	ing.logger.Info("successfully subscribed, entering read loop", "tickers", ing.tickers)
	// read messages in a loop
	for {
		select {
		case <-ctx.Done(): // if context is cancelled, we should exit the read loop and close the connection
			return ctx.Err()
		default: // otherwise, read the next message from the WebSocket
			_, rawMessage, err := conn.ReadMessage() // ReadMessage blocks until a message is received or an error occurs
			if err != nil {
				return fmt.Errorf("read error: %w", err)
			}
			// parse and forward the message
			ing.handleMessage(rawMessage)
		}
	}
}

// parses a raw WebSocket message from Kalshi and pushes it to event channel
func (ing *Ingester) handleMessage(rawMessage []byte) {
	// parse outer envelope to check the message type and route accordingly
	var msg wsMessage
	// json.Unmarshal parses JSON data into a Go struct
	if err := json.Unmarshal(rawMessage, &msg); err != nil {
		ing.logger.Error("failed to parse WebSocket message", "error", err, "rawMessage", string(rawMessage))
		return
	}
	// determine event type
	var eventType string
	switch msg.Type {
	case "orderbook_snapshot":
		eventType = "snapshot"
	case "orderbook_delta":
		eventType = "delta"
	default:
		ing.logger.Debug("ignoring message type", "type", msg.Type)
		return
	}
	// parse inner payload
	var data wsOrderBookData
	if err := json.Unmarshal(msg.Msg, &data); err != nil {
		ing.logger.Error("failed to parse orderbook data", "error", err)
		return
	}
	// check sequence numbers for gaps
	if lastSeq, exists := ing.seqNums[data.MarketTicker]; exists {
		if msg.Seq != lastSeq+1 && eventType == "delta" {
			ing.logger.Warn("sequence number gap detected", "marketTicker", data.MarketTicker, "expected", lastSeq+1, "received", msg.Seq, "missed", msg.Seq - lastSeq - 1)
			// TODO: trigger a REST snapshot resync here
		}
	}
	ing.seqNums[data.MarketTicker] = msg.Seq
	// convert raw [[price, quantity], ...] arrays into []PriceLevel structs
	event := models.OrderBookEvent{
		Type: eventType,
		MarketTicker: data.MarketTicker,
		SeqNum: msg.Seq,
		YesBids: parseLevels(data.YesBids),
		NoBids: parseLevels(data.NoBids),
		ReceivedAt: time.Now(),
	}
	// push to the channel
	ing.eventChannel <- event
	ing.logger.Debug("event forwarded", "type", eventType, "ticker", data.MarketTicker, "seqNum", msg.Seq, "yesLevels", len(event.YesBids), "noLevels", len(event.NoBids))
}

// converts Kalshi's [[price, quantity], ...] format into []PriceLevel
func parseLevels(rawLevels [][]int) []models.PriceLevel {
	levels := make([]models.PriceLevel, 0, len(rawLevels))
	for _, pair := range rawLevels {
		if len(pair) != 2 {
			continue // skip malformed entries
		}
		levels = append(levels, models.PriceLevel{
			Price: pair[0],
			Quantity: pair[1],
		})
	}
	return levels
}