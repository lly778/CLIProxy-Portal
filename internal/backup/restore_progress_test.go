package backup

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

type progressRestoreService struct {
	restoreService
	t   *testing.T
	dir string
}

func (s *progressRestoreService) requirePhase(phase string) {
	s.t.Helper()
	status, err := LoadRestoreStatus(s.dir)
	if err != nil || !status.Running || status.Phase != phase || status.PhaseStartedAt.IsZero() || !strings.Contains(status.Message, restorePhases[phase]) {
		s.t.Fatalf("phase at service operation: %+v, %v; want %s", status, err, phase)
	}
}

func (s *progressRestoreService) Verify(ctx context.Context, p PortalRestorePolicy) error {
	s.requirePhase("verify_targets")
	return s.restoreService.Verify(ctx, p)
}

func (s *progressRestoreService) Stop(ctx context.Context) error {
	phase := "stop_services"
	if s.stops > 0 {
		phase = "rollback"
	}
	s.requirePhase(phase)
	return s.restoreService.Stop(ctx)
}

func (s *progressRestoreService) Start(ctx context.Context) error {
	phase := "start_services"
	if s.starts > 0 {
		phase = "rollback"
	}
	s.requirePhase(phase)
	return s.restoreService.Start(ctx)
}

func (s *progressRestoreService) Healthy(ctx context.Context) error {
	phase := "health_check"
	if s.health > 0 {
		phase = "rollback"
	}
	s.requirePhase(phase)
	return s.restoreService.Healthy(ctx)
}

func TestRestoreProgressMatchesServiceWorkAndRollback(t *testing.T) {
	for _, failHealth := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[failHealth], func(t *testing.T) {
			state, restored, p := restoreFixture(t)
			status := RestoreStatus{Running: true, StartedAt: time.Now().UTC()}
			ctx := withRestoreProgress(t.Context(), state, &status)
			persist := ctx.Value(restoreProgressKey{}).(func(string))
			var phases []string
			ctx = context.WithValue(ctx, restoreProgressKey{}, func(phase string) {
				phases = append(phases, phase)
				persist(phase)
			})
			svc := &progressRestoreService{t: t, dir: state, restoreService: restoreService{healthFail: failHealth}}
			_, err := replacePortalDatabase(ctx, state, restored, p, svc, DatabaseRestoreMode)
			if (err != nil) != failHealth {
				t.Fatalf("restore: %v", err)
			}
			want := []string{"verify_targets", "stage_data", "stop_services", "rollback_snapshot", "replace_data", "start_services", "health_check"}
			if failHealth {
				want = append(want, "rollback")
				if status.FailedPhase != "health_check" {
					t.Fatalf("lost failing stage: %+v", status)
				}
				checkRestoreDB(t, p.Database, "live", false)
			} else {
				checkRestoreDB(t, p.Database, "backup", true)
			}
			if !reflect.DeepEqual(phases, want) {
				t.Fatalf("phases %v; want %v", phases, want)
			}
		})
	}
}

func TestRestoreProgressRejectsUnknownTextAndIsOptional(t *testing.T) {
	dir := t.TempDir()
	s := RestoreStatus{Running: true}
	ctx := withRestoreProgress(t.Context(), dir, &s)
	reportRestorePhase(ctx, "decrypt")
	before, err := LoadRestoreStatus(dir)
	if err != nil {
		t.Fatal(err)
	}
	reportRestorePhase(ctx, "secret-from-untrusted-output")
	after, err := LoadRestoreStatus(dir)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("unknown progress accepted: %+v %v", after, err)
	}
	// Offline restores and backup verification have no progress sink.
	reportRestorePhase(t.Context(), "verify_archive")
}
