package agy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAGYStatusCoalescesPollsAndNeverStartsModels(t *testing.T) {
	var calls atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/internal/status" || r.Method != "GET" {
			t.Error("status poll requested expensive endpoint")
		}
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ready":true,"active_sessions":2,"waiting_sessions":1,"sessions":3}`))
	}))
	defer worker.Close()
	client, err := NewClient(worker.URL, strings.Repeat("s", 64))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := client.Status(t.Context())
			if err != nil || v.ActiveSessions != 2 || v.WaitingSessions != 1 {
				t.Error("status counters lost", err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("duplicate worker reads: %d", calls.Load())
	}
	if _, err = client.Status(t.Context()); err != nil || calls.Load() != 1 {
		t.Fatal("page / poll did not share cache")
	}
}
