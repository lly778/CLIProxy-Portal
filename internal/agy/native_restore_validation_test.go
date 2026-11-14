package agy

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestKnownNativeLoadCannotSilentlyRestoreMissingOrEmptyHistory(t *testing.T) {
	for _, state := range []string{"missing", "empty", "corrupt", "complete"} {
		t.Run(state, func(t *testing.T) {
			m, s := recoveryFixture(t, func(*session, frame) any { t.Error("validation invoked a prompt"); return nil })
			var loads atomic.Int64
			m.proc = diskTestProcess(t, func(f frame) any {
				if f.Method == "session/load" {
					loads.Add(1)
				}
				return map[string]any{"sessionId": s.ID, "configOptions": s.options}
			})
			m.proc.evidence = newRuntimeEvidence(verifiedAGY153, "agy", "1.5.3", 1, nil)
			m.sessions = map[string]*session{}
			s.proc = nil
			path := filepath.Join(m.cfg.Home, "agy", "conversations", s.ID+".db")
			if state != "missing" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if state == "corrupt" {
					if err := os.WriteFile(path, []byte("invalid fixture database"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					db, err := sql.Open("sqlite", filepath.ToSlash(path))
					if err != nil {
						t.Fatal(err)
					}
					if _, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY)`); err != nil {
						t.Fatal(err)
					}
					if state == "complete" {
						if _, err = db.Exec(`INSERT INTO steps(idx) VALUES(0)`); err != nil {
							t.Fatal(err)
						}
					}
					db.Close()
				}
			}
			_, err := m.restoreNativeSession(t.Context(), s, request{Model: s.model})
			switch state {
			case "complete":
				if err != nil || loads.Load() != 1 {
					t.Fatal("valid native history rejected", err)
				}
			case "corrupt":
				if err == nil || isMissingSession(err) || loads.Load() != 0 {
					t.Fatal("corruption treated as missing or loaded", err)
				}
			default:
				if !isMissingSession(err) || loads.Load() != 0 {
					t.Fatal("empty/missing native history accepted as restored", err)
				}
			}
			if state == "missing" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("preflight created a phantom native database")
				}
			}
		})
	}
}

func TestNativeLoadReturningDifferentIdentityKeepsOldRecoveryRecord(t *testing.T) {
	m, s := recoveryFixture(t, func(*session, frame) any { return nil })
	d, err := openSessionDiskCache(t.TempDir(), strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer d.db.Close()
	m.disk = d
	s.items = evidenceTranscript()[:3]
	s.history = requestFingerprints(s.items)
	if err = d.put(snapshotSession(s), true); err != nil {
		t.Fatal(err)
	}
	m.sessions = map[string]*session{}
	s.proc = nil
	m.proc = diskTestProcess(t, func(frame) any {
		return map[string]any{"sessionId": "unexpected-new-native", "configOptions": s.options}
	})
	if _, err = m.restoreNativeSession(t.Context(), s, request{Model: s.model}); err == nil {
		t.Fatal("different native identity accepted")
	}
	if old, err := d.find(s.owner, request{Previous: "previous"}, nil); err != nil || old == nil {
		t.Fatal("different-identity failure destroyed old recovery point", err)
	}
}
