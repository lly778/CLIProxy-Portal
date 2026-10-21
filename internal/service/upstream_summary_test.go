package service

import (
	"context"
	"errors"
	"testing"

	"cliproxy-portal/internal/cpamp"
)

type summaryCPAMP struct {
	*channelCPAMP
	reads             int
	fileErr, modelErr error
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
