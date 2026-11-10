package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cliproxy-portal/internal/agy"
)

func TestAGYSignedImagesUsePrivateWorkerWithoutModelAdmission(t *testing.T) {
	secret := strings.Repeat("secret", 8)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.URL.Path != "/internal/images/session/portal_image_test.png" || r.URL.Query().Get("sig") != "signed" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, max-age=300")
		_, _ = io.WriteString(w, "actual-image-bytes")
	}))
	defer worker.Close()
	client, err := agy.NewClient(worker.URL, secret)
	if err != nil {
		t.Fatal(err)
	}
	g := &Gateway{AGY: client}
	w := httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/agy/images/session/portal_image_test.png?sig=signed", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.String() != "actual-image-bytes" {
		t.Fatal("image was sent through model authentication or lost its private worker request")
	}
	w = httptest.NewRecorder()
	g.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/agy/images/session/portal_image_test.png?sig=signed", nil))
	if w.Code != 405 {
		t.Fatal("image endpoint accepted non-read operation")
	}
}
