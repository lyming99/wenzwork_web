package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/wenzwork/wenzwork-web/server/internal/relaymaintenance"
	"github.com/wenzwork/wenzwork-web/server/internal/relaymanagement"
)

// runRelayMaintenance absorbs the remaining topology-operation work into
// Host. There is intentionally no separate relay-operator process and no
// Redis credential/fence projection loop.
func runRelayMaintenance(ctx context.Context, store *relaymanagement.Store, log *slog.Logger) {
	worker, err := relaymaintenance.NewWorker(store, relaymaintenance.EndpointValidator{Identities: store})
	if err != nil {
		log.Error("Relay Host maintenance startup failed", "error", err)
		return
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	nextLeasePass := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		passContext, cancel := context.WithTimeout(ctx, 12*time.Second)
		if !time.Now().Before(nextLeasePass) {
			if expired, leaseErr := store.ExpireLeases(passContext); leaseErr != nil {
				log.Error("Relay Host lease expiration pass failed", "error", leaseErr)
			} else if expired > 0 {
				log.Info("Relay Host leases expired", "count", expired)
			}
			nextLeasePass = time.Now().Add(10 * time.Second)
		}
		for processed := 0; processed < 20; processed++ {
			didWork, operationErr := worker.ProcessOne(passContext)
			if operationErr != nil {
				log.Error("Relay Host operation failed", "error", operationErr)
				break
			}
			if !didWork {
				break
			}
		}
		cancel()
	}
}
