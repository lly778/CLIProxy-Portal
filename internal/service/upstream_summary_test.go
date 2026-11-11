package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

type summaryCPAMP struct {
	*channelCPAMP
	reads             int
	fileErr, modelErr error
}

func TestAGYEnabledUpstreamsSharesAccountAndModelsWithoutCPARegistration(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.SaveAGYSettings(t.Context(), agy.Settings{Models: []agy.Model{{ID: "gemini"}, {ID: "disabled"}}, Rules: []agy.ModelRule{{ID: "gemini", Enabled: true}, {ID: "disabled"}}}, 0); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	var loggedIn atomic.Bool
	loggedIn.Store(true)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if r.URL.Path != "/internal/account" {
			t.Error("overview fetched catalog/quota instead of shared account metadata")
		}
		if loggedIn.Load() {
			w.Write([]byte(`{"logged_in":true}`))
		} else {
			w.Write([]byte(`{"logged_in":false}`))
		}
	}))
	defer worker.Close()
	f := &summaryCPAMP{channelCPAMP: newChannelCPAMP()}
	k := NewKeys(st, f)
	k.AGY, _ = agy.NewClient(worker.URL, strings.Repeat("s", 64))
	ctx := cpamp.WithUpstreamReadCache(t.Context())
	for i := 0; i < 2; i++ {
		rows, err := k.EnabledUpstreams(ctx)
		if err != nil || len(rows) != 3 {
			t.Fatalf("missing CLI source: %+v %v", rows, err)
		}
		if rows[0].Channel != "codex" || rows[1].Channel != "antigravity" || rows[2].Channel != "agy" {
			t.Fatalf("inconsistent provider order: %+v", rows)
		}
		found := false
		for _, v := range rows {
			if v.Channel == "agy" {
				found = true
				if v.Accounts != 1 || !v.ModelsReady || len(v.Models) != 1 || v.Models[0] != "agy/gemini" {
					t.Fatalf("wrong active model projection: %+v", v)
				}
			}
		}
		if !found {
			t.Fatal("AGY not listed")
		}
	}
	if reads.Load() != 1 || f.reads != 1 {
		t.Fatalf("repeated source reads: account=%d CPA=%d", reads.Load(), f.reads)
	}
	// A CPA outage must not hide the separately configured CLI account.
	k.InvalidateUpstreamReadCache()
	f.fileErr = errors.New("CPA unavailable")
	rows, err := k.EnabledUpstreams(ctx)
	if err == nil || len(rows) != 1 || rows[0].Channel != "agy" {
		t.Fatalf("partial source hidden: %+v %v", rows, err)
	}
	loggedIn.Store(false)
	k.InvalidateUpstreamReadCache()
	rows, _ = k.EnabledUpstreams(ctx)
	if len(rows) != 0 {
		t.Fatal("logged-out CLI still counted")
	}
}

func (f *summaryCPAMP) ListAuthFiles(ctx context.Context) ([]cpamp.AuthFile, error) {
	f.reads++
	if f.fileErr != nil {
		return nil, f.fileErr
	}
	return f.channelCPAMP.ListAuthFiles(ctx)
}
func (f *summaryCPAMP) ListOAuthModelDefinitions(ctx context.Context, channel string) ([]cpamp.OAuthModelDefinition, error) {
	if f.modelErr != nil {
		return nil, f.modelErr
	}
	return f.channelCPAMP.ListOAuthModelDefinitions(ctx, channel)
}

func TestEnabledUpstreamsCountsEffectiveConfigurationAndReportsUnknownModels(t *testing.T) {
	f := &summaryCPAMP{channelCPAMP: newChannelCPAMP()}
	f.files = append(f.files, f.files[0], cpamp.AuthFile{Name: "off.json", Provider: "claude", Disabled: true}, cpamp.AuthFile{Provider: "codex"})
	f.excluded["antigravity"] = []string{"anti-*"}
	k := NewKeys(nil, f)
	rows, err := k.EnabledUpstreams(t.Context())
	if err != nil || f.reads != 1 || len(rows) != 2 {
		t.Fatalf("summary = %+v, reads=%d, err=%v", rows, f.reads, err)
	}
	for _, row := range rows {
		if row.Accounts != 1 || len(row.Models) != 1 || row.Models[0] != "shared" || !row.ModelsReady {
			t.Fatalf("disabled, duplicate or excluded configuration counted: %+v", row)
		}
	}
	f.modelErr = errors.New("catalog unavailable")
	rows, err = k.EnabledUpstreams(t.Context())
	if err != nil || len(rows) != 2 || rows[0].ModelsReady || rows[1].ModelsReady {
		t.Fatalf("model failures presented as known zero: %+v %v", rows, err)
	}
	f.fileErr = errors.New("credentials unavailable")
	if rows, err = k.EnabledUpstreams(t.Context()); err == nil || len(rows) != 0 {
		t.Fatalf("failed discovery presented as empty success: %+v %v", rows, err)
	}
}
