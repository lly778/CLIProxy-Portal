package store

import (
	"path/filepath"
	"testing"
)

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
	if e != nil || p.Revision != 2 || p.Global != 3 || p.MemoryMiB != 768 {
		t.Fatal("policy not durable", p, e)
	}
}
