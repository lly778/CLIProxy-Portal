package agy

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAGYCompletionUsesACPAndNativeDatabaseOnlySuppliesUsage(t *testing.T) {
	for _, stop := range []string{"end_turn", "stop", "cancelled"} {
		t.Run(stop, func(t *testing.T) {
			var db *sql.DB
			m, _ := recoveryFixture(t, func(s *session, f frame) any {
				// No step_payload column: native errors must never be read to
				// override ACP, even when all generated tokens are reasoning.
				if _, err := db.Exec(`INSERT INTO steps VALUES(1,15,3,?),(2,17,3,NULL)`, usageProto(2, 100, 3, 342, 9, 342)); err != nil {
					t.Error(err)
				}
				return map[string]any{"stopReason": stop}
			})
			dir := filepath.Join(m.cfg.Home, "agy", "conversations")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			var err error
			db, err = sql.Open("sqlite", filepath.ToSlash(filepath.Join(dir, "native.db")))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB)`); err != nil {
				t.Fatal(err)
			}
			w := recoveryRequest(m, `{"model":"agy/test","previous_response_id":"previous","input":"continue"}`)
			if stop == "cancelled" {
				if !strings.Contains(w.Body.String(), "agent_incomplete") || strings.Contains(w.Body.String(), `"status":"completed"`) {
					t.Fatalf("ACP failure hidden: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"completed"`) || !strings.Contains(w.Body.String(), `"output_tokens":342`) {
				t.Fatalf("native error overrode ACP or usage was lost: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
