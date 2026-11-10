package agy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAGYImagePreviewFitsCodexMarkdownBudgetAndRetainsDownload(t *testing.T) {
	picture := image.NewNRGBA(image.Rect(0, 0, 1024, 512))
	rng := rand.New(rand.NewSource(42))
	_, _ = rng.Read(picture.Pix)
	var raw bytes.Buffer
	if err := png.Encode(&raw, picture); err != nil {
		t.Fatal(err)
	}
	preview, err := imagePreview(raw.Bytes())
	if err != nil || len(preview) > 64<<10 || !strings.HasPrefix(preview, "data:image/png;base64,iVBORw0KGgo") {
		t.Fatal("preview missing or exceeds the client inline-image budget", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(preview, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || cfg.Width > 192 || cfg.Height*2 != cfg.Width {
		t.Fatal("preview dimensions invalid", cfg, err)
	}
	for _, text := range []string{"![cat](part_0)", "Image generated."} {
		o := newResponse(httptest.NewRecorder(), true, "agy/test")
		o.imageRefs = []string{preview}
		o.imageLinks = []string{"https://api.example.test/image.png?sig=scoped"}
		for _, chunk := range []string{text[:3], text[3:]} {
			if err := o.text(chunk, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := o.finish(Usage{}); err != nil {
			t.Fatal(err)
		}
		if strings.Count(o.textValue, preview) != 1 || strings.Contains(o.textValue, "part_0") || !strings.Contains(o.textValue, "[下载原图]("+o.imageLinks[0]+")") {
			t.Fatal("missing inline preview or original download, or duplicated preview")
		}
	}
}

func TestAGYGeneratedImageIsPNGAndItsURLIsScoped(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{B: 255, A: 255})
	var jpegBytes bytes.Buffer
	if err := jpeg.Encode(&jpegBytes, img, nil); err != nil {
		t.Fatal(err)
	}
	data, size, err := imagePNG(jpegBytes.Bytes())
	if err != nil || size != "2x2" || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("JPEG did not become a real PNG")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "agy", "brain", "owner-session")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 40)
	link, err := storePublicImage(home, "https://api.example.test/v1", secret, "private-owner", "owner-session", data)
	if err != nil || strings.Contains(link, "private-owner") {
		t.Fatal("image URL missing or exposes owner")
	}
	u, _ := url.Parse(link)
	m := &Manager{cfg: RuntimeConfig{Home: home, Secret: secret}}
	read := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		m.Handler().ServeHTTP(w, r)
		return w
	}
	path := strings.Replace(u.RequestURI(), "/v1/agy/images/", "/internal/images/", 1)
	w := read(path)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("signed image could not be fetched")
	}
	if read(strings.Replace(path, "owner-session", "other-session", 1)).Code != 404 {
		t.Fatal("signature accepted for another session")
	}
	if read(strings.Replace(path, "sig=", "sig=bad", 1)).Code != 404 {
		t.Fatal("tampered image signature accepted")
	}
	if read(strings.Split(path, "?")[0]).Code != 404 {
		t.Fatal("unsigned image exposed")
	}
}

func TestAGYInternalImagePartsAreRewrittenAcrossStreamChunks(t *testing.T) {
	link := "https://api.example.test/v1/agy/images/session/image.png?sig=valid"
	w := httptest.NewRecorder()
	o := newResponse(w, true, "agy/test")
	o.imageRefs = []string{link}
	for _, chunk := range []string{"Generated: ![blue cat](pa", "rt_", "0", ") text ending p"} {
		if err := o.text(chunk, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.finish(Usage{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(o.textValue, "part_0") || o.textValue != "Generated: ![blue cat]("+link+") text ending p" {
		t.Fatal("internal part reference remained broken")
	}
	// The final response and streamed deltas must contain identical rendered text.
	var deltas strings.Builder
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.output_text.delta" {
			deltas.WriteString(event["delta"].(string))
		}
	}
	if deltas.String() != o.textValue {
		t.Fatal("stream and completed text disagree")
	}
}

func TestAGYImageDownloadLinkDoesNotInvalidatePixelHistory(t *testing.T) {
	base := map[string]any{"type": "image_generation_call", "result": "actual-pixels"}
	linked := map[string]any{"type": "image_generation_call", "result": "actual-pixels", "result_url": "https://api.example.test/image?sig=transport"}
	if itemFingerprint(base) != itemFingerprint(linked) {
		t.Fatal("optional image link invalidated history")
	}
	linked["result"] = "different-pixels"
	if itemFingerprint(base) == itemFingerprint(linked) {
		t.Fatal("changed image pixels matched old history")
	}
}
