package agy

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func usageProto(values ...uint64) []byte {
	var p []byte
	for i := 0; i < len(values); i += 2 {
		p = binary.AppendUvarint(p, values[i]<<3)
		p = binary.AppendUvarint(p, values[i+1])
	}
	return append(binary.AppendUvarint([]byte{9<<3 | 2}, uint64(len(p))), p...)
}

func TestAGYNativeUsageDecoderUsesInclusiveTotals(t *testing.T) {
	for _, test := range []struct {
		name string
		b    []byte
		want Usage
	}{
		{"all", usageProto(2, 100, 3, 40, 4, 10, 5, 200, 9, 30), Usage{Known: true, Input: 310, Output: 40, Cache: 210, Reasoning: 30, Total: 350}},
		{"sparse", usageProto(2, 840, 3, 110, 9, 109), Usage{Known: true, Input: 840, Output: 110, Reasoning: 109, Total: 950}},
		{"zero-output", usageProto(2, 20), Usage{Known: true, Input: 20, Total: 20}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeNativeUsage(test.b)
			if err != nil || got != test.want {
				t.Fatalf("usage=%+v err=%v", got, err)
			}
		})
	}
	for _, b := range [][]byte{nil, {0x4a, 0}, {0x4a, 0x80}, usageProto(1, 123), usageProto(2, math.MaxUint64), usageProto(2, math.MaxInt64, 3, 1), usageProto(2, 1, 3, 1, 9, 2), append(usageProto(2, 10), usageProto(3, 10)...)} {
		if u, err := decodeNativeUsage(b); err == nil || u.Known {
			t.Fatalf("invalid usage trusted: %+v", u)
		}
	}
}

func TestAGYNativeUsageReadsOnlyNewCompletedCallsAndDoesNotCreateFiles(t *testing.T) {
	home := t.TempDir()
	id := "native-usage-test"
	if _, err := openNativeUsage(home, id); err == nil {
		t.Fatal("opened missing file")
	}
	dir := filepath.Join(home, "agy", "conversations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".db")
	db, err := sql.Open("sqlite", filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`CREATE TABLE steps(idx INTEGER PRIMARY KEY,step_type INTEGER,status INTEGER,metadata BLOB,step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	add := func(idx, typ, status int, b []byte) {
		t.Helper()
		if _, e := db.Exec(`INSERT INTO steps(idx,step_type,status,metadata) VALUES(?,?,?,?)`, idx, typ, status, b); e != nil {
			t.Fatal(e)
		}
	}
	add(1, 15, 3, usageProto(2, 999, 3, 1))
	cursor, err := nativeUsageCursor(t.Context(), home, id)
	if err != nil || cursor != 1 {
		t.Fatalf("cursor %d err %v", cursor, err)
	}
	add(2, 1, 3, nil)
	add(3, 15, 1, nil)
	add(4, 15, 3, usageProto(2, 100, 3, 40, 5, 200, 9, 30))
	add(6, 15, 3, usageProto(2, 20, 3, 10))
	r, err := openNativeUsage(home, id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.db.Close()
	u, next, calls, err := r.after(t.Context(), cursor)
	if err != nil || next != 6 || calls != 2 || u != (Usage{Known: true, Input: 320, Output: 50, Cache: 200, Reasoning: 30, Total: 370}) {
		t.Fatalf("new calls=%+v next=%d calls=%d err=%v", u, next, calls, err)
	}
	if _, err = r.db.Exec(`DELETE FROM steps`); err == nil {
		t.Fatal("usage reader writable")
	}
	s := &session{ID: id, life: context.Background(), usageCursor: cursor}
	m := &Manager{cfg: RuntimeConfig{Home: home}}
	if got := m.consumeUsage(s); got != u {
		t.Fatalf("first HTTP tool step: %+v", got)
	}
	add(7, 15, 3, usageProto(2, 60, 3, 8, 9, 4))
	if got := m.consumeUsage(s); got.Total != 68 || got.Input != 60 {
		t.Fatalf("tool return double counted: %+v", got)
	}
	// A new native turn / restored session starts after all existing history.
	m.startUsage(s)
	add(8, 15, 3, usageProto(2, 80, 3, 5))
	if got := m.consumeUsage(s); got.Total != 85 {
		t.Fatalf("old turn included: %+v", got)
	}
	if _, _, calls, err := r.after(t.Context(), 8); err != nil || calls != 0 {
		t.Fatalf("completed calls repeated: %d %v", calls, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "missing.db")); !os.IsNotExist(err) {
		t.Fatal("unexpected file created")
	}
	if _, err = openNativeUsage(home, "../other"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestAGYTrackerPreservesUsageAndEffectiveEffort(t *testing.T) {
	body := `{"id":"test","status":"completed","reasoning":{"effort":"high"},"usage":{"input_tokens":310,"output_tokens":40,"total_tokens":350,"input_tokens_details":{"cached_tokens":210},"output_tokens_details":{"reasoning_tokens":30}}}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			tracker := &Tracker{Stream: stream}
			wire := body
			if stream {
				wire = "data: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
			}
			for _, v := range []byte(wire) {
				tracker.Feed([]byte{v})
			}
			tracker.Finish()
			if !tracker.Completed || tracker.Effort != "high" || tracker.Usage.Total != 350 || tracker.Usage.Cache != 210 || !tracker.Usage.Known {
				t.Fatalf("lost metadata: %+v", tracker)
			}
		})
	}
}
