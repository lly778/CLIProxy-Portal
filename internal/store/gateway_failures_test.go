package store

import (
	"testing"
	"time"
)

func TestInterruptedUploadStagesAndLegacyMigration(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 10, 12, 13, 37, 4, 0, time.UTC)
	write := int64(117502)
	row := GatewayRequestTiming{ID: "interrupted", UserID: "owner", APIKeyHash: "hash", StartedAt: start, EndedAt: start.Add(118003 * time.Millisecond), TotalMS: 118003, ResponseStartedMS: &write, StatusCode: 502, FailureReason: "upload_interrupted"}
	if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GatewayTimingByID(t.Context(), row.ID, "hash"); err != nil || got.RequestReadMS != nil || got.FailureReason != row.FailureReason {
		t.Fatalf("raw boundary lost: %+v, %v", got, err)
	}
	if _, err := s.GatewayTimingByID(t.Context(), row.ID, "other-hash"); err == nil {
		t.Fatal("timing exposed to another key")
	}
	// An old row has no error metadata, but its retained boundaries can recover
	// reception/error handling and the error-response tail, never an EOF time.
	if _, err := s.db.Exec(`UPDATE gateway_request_timings SET failure_reason='',status_code=0 WHERE id=?`, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE gateway_timing_samples SET request_read_ms=NULL WHERE id=?`, row.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.migrate(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		rows, count, err := s.GatewayUntracedFailures(t.Context(), start, start.Add(time.Hour), "owner", 100)
		if err != nil || count != 1 || len(rows) != 1 || rows[0].RequestReadMS != nil || rows[0].FailureReason != "upload_interrupted_legacy" {
			t.Fatalf("legacy failure: %+v %d %v", rows, count, err)
		}
		totals, err := s.GatewayTimingTotals(t.Context(), start, start.Add(time.Hour), "owner", start, time.Hour)
		if err != nil || len(totals) != 1 {
			t.Fatalf("totals: %+v %v", totals, err)
		}
		got := totals[0]
		if got.Samples != 1 || got.StageSamples != 1 || got.UploadMS != 117502 || got.WaitMS != 0 || got.ResponseMS != 501 || got.TotalMS != 118003 {
			t.Fatalf("legacy stage loss/double count: %+v", got)
		}
	}
	row.ID = "no-response"
	row.ResponseStartedMS = nil
	if err := s.SaveGatewayRequestTiming(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	stages := GatewayTimingStages(row)
	if stages.RequestReadMS == nil || *stages.RequestReadMS != row.TotalMS || stages.ResponseStartedMS == nil || *stages.ResponseStartedMS != row.TotalMS {
		t.Fatalf("connection closure lost upload time: %+v", stages)
	}
}
