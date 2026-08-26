package remotepoc

import (
	"context"
	"testing"
	"time"
)

func TestScenarioCoversAllocationProofDedupAndMigration(t *testing.T) {
	outcome, err := Run(context.Background(), time.Date(2026, 8, 5, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.InitialCell == outcome.MigratedCell || outcome.InitialNode == outcome.MigratedNode {
		t.Fatalf("migration did not move Cell/Node: %+v", outcome)
	}
	if outcome.AssignmentVersionAfter != outcome.AssignmentVersionBefore+1 || outcome.ConnectionEpochAfter <= outcome.ConnectionEpochBefore {
		t.Fatalf("fences did not advance: %+v", outcome)
	}
	if !outcome.OldRouteFenced || !outcome.GoAwayHandled || outcome.CommandDeliveries != 100 || outcome.CommandSideEffects != 1 || outcome.FileRoundTripBytes == 0 {
		t.Fatalf("scenario outcome = %+v", outcome)
	}
}
