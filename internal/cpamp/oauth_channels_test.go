package cpamp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListOAuthChannelsMergesConfiguredSelectors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-key" {
			t.Error("missing management authorization")
		}
		switch r.URL.Path {
		case pathOAuthModelAlias:
			_, _ = w.Write([]byte(`{"oauth-model-alias":{"codex":[],"Antigravity":[{"name":"model","alias":"public"}],"plugin-oauth":null}}`))
		case pathOAuthExcluded:
			_, _ = w.Write([]byte(`{"claude":["hidden"],"codex":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "admin-key")
	if err != nil {
		t.Fatal(err)
	}
	channels, err := client.ListOAuthChannels(t.Context())
	if err != nil || !reflect.DeepEqual(channels, []string{"antigravity", "claude", "codex", "plugin-oauth"}) {
		t.Fatalf("channels=%v err=%v", channels, err)
	}
}
