package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResourcePolicyMigratesMiBToMBOnceWithoutChangingLimits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{"revision":11,"mode":"enforce","global":10,"per_user":5,"body_mib":10,"cpu_percent":90,"memory_mib":1280}`
	_, err = s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", resourcePolicyKey, legacy)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err = Open(path, true)
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.ResourcePolicy(t.Context())
		if err != nil || p.Revision != 11 || p.Mode != "enforce" || p.Global != 10 || p.PerUser != 5 || p.MemoryMB != 1342.17728 || p.BodyLimitBytes() != 10*1048576 {
			t.Fatalf("migration changed byte limits or policy on open %d: %+v, %v", i, p, err)
		}
		var raw string
		if err = s.db.QueryRow("SELECT value FROM settings WHERE key=?", resourcePolicyKey).Scan(&raw); err != nil || strings.Contains(raw, "_mib") || !strings.Contains(raw, "\"memory_mb\":1342.17728") {
			t.Fatal("stored policy is not in MB", raw, err)
		}
		s.Close()
	}
}

func TestResourcePolicyPersistenceAndStaleRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	s, e := Open(path, true)
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.ResourcePolicy(t.Context())
	if e != nil || p.Mode != "observe" || p.Revision != 1 {
		t.Fatal(p, e)
	}
	old := p
	p.Global = 3
	if e = s.SaveResourcePolicy(t.Context(), p); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveResourcePolicy(t.Context(), old); e == nil {
		t.Fatal("stale policy replaced newer configuration")
	}
	s.Close()
	s, e = Open(path, true)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	p, e = s.ResourcePolicy(t.Context())
	if e != nil || p.Revision != 2 || p.Global != 3 || p.MemoryMB != 768 {
		t.Fatal("policy not durable", p, e)
	}
}
