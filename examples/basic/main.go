// Every kind of log, then a panic. Run with your ingest-only key in the environment:
//
//	ZHARP_API_KEY=your-ingest-only-key go run ./examples/basic
package main

import (
	"errors"
	"fmt"
	"log/slog"

	zharp "github.com/zharp-io/zharp-go-lib"
)

var errDeclined = errors.New("card declined")

func charge(orderID int) error {
	return fmt.Errorf("charge order %d: %w", orderID, errDeclined)
}

func main() {
	zharp.Init(zharp.Config{Service: "go-sample", Environment: "development", AppVersion: "1.0.0"})
	defer zharp.Close()
	defer zharp.Recover()

	zharp.Info("Checkout opened", zharp.Fields{"cart_items": 3, "currency": "GHS"})
	zharp.Warn("Slow network detected", zharp.Fields{"latency_ms": 3200})

	if err := charge(99); err != nil {
		zharp.Error("Payment failed", zharp.Fields{"order_id": 99, "err": err})
	}

	// Go's own structured logging, through the handler.
	logger := slog.New(zharp.NewSlogHandler(nil))
	logger.Warn("Stock low", "sku", "SKU-1")

	// A panic: sent as fatal with its stack, then the program dies of it as usual.
	var stock map[string]int
	stock["SKU-1"]--
}
