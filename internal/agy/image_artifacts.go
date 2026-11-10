package agy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Codex desktop normalizes untyped generation results as PNG. Return actual
// PNG bytes, not JPEG bytes carrying a PNG label. Bound decoded dimensions too.
func imagePNG(data []byte) ([]byte, string, error) {
	bad := fail(502, "invalid_image_result", "生成的图片格式或尺寸不支持回传")
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 4<<20 {
		return nil, "", bad
	}
	size := strconv.Itoa(cfg.Width) + "x" + strconv.Itoa(cfg.Height)
	if http.DetectContentType(data) == "image/png" {
		return data, size, nil
	}
	picture, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", bad
	}
	var out bytes.Buffer
	if (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&out, picture) != nil || out.Len() > 4<<20 {
		return nil, "", bad
	}
	return out.Bytes(), size, nil
}

// Markdown previews have a smaller payload budget than native image items in
// Codex. Embed a bounded PNG thumbnail; keep original pixels in the native item
// and the separately signed download link.
func imagePreview(data []byte) (string, error) {
	picture, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	bounds := picture.Bounds()
	const prefix = "data:image/png;base64,"
	for _, edge := range []int{192, 128, 96} {
		width, height := bounds.Dx(), bounds.Dy()
		if width > edge || height > edge {
			if width >= height {
				height, width = max(1, height*edge/width), edge
			} else {
				width, height = max(1, width*edge/height), edge
			}
		}
		thumb := image.NewNRGBA(image.Rect(0, 0, width, height))
		for y := range height {
			for x := range width {
				thumb.Set(x, y, picture.At(bounds.Min.X+x*bounds.Dx()/width, bounds.Min.Y+y*bounds.Dy()/height))
			}
		}
		var out bytes.Buffer
		if err := png.Encode(&out, thumb); err != nil {
			return "", err
		}
		if len(prefix)+base64.StdEncoding.EncodedLen(out.Len()) <= 64<<10 {
			return prefix + base64.StdEncoding.EncodeToString(out.Bytes()), nil
		}
	}
	return "", fail(502, "image_preview_failed", "图片预览生成失败")
}

func publicImageBase(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	return strings.TrimRight(u.String(), "/")
}

func imageMAC(secret, value string) string {
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(h, "cliproxy-portal/agy/image/v1\x00"+value)
	return hex.EncodeToString(h.Sum(nil))
}

func storePublicImage(home, base, secret, owner, id string, data []byte) (string, error) {
	base = publicImageBase(base)
	if base == "" {
		return "", nil
	}
	if !safeNativeSessionID(id) || len(secret) < 32 || owner == "" {
		return "", fail(502, "invalid_image_result", "图片所属会话无效")
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return "", err
	}
	defer root.Close()
	brain, err := root.OpenRoot(filepath.Join("agy", "brain", id))
	if err != nil {
		return "", err
	}
	defer brain.Close()
	name := newID("portal_image_") + ".png"
	f, err := brain.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = brain.Remove(name)
		return "", fail(502, "image_store_failed", "图片保存失败")
	}
	expires := strconv.FormatInt(time.Now().Add(sessionRetention).Unix(), 10)
	tag := imageMAC(secret, "owner\x00"+owner)
	sig := imageMAC(secret, tag+"\x00"+id+"\x00"+name+"\x00"+expires)
	query := url.Values{"expires": {expires}, "owner": {tag}, "sig": {sig}}
	return base + "/v1/agy/images/" + id + "/" + name + "?" + query.Encode(), nil
}

func (m *Manager) imageHTTP(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("session"), r.PathValue("name")
	q := r.URL.Query()
	expiration := q.Get("expires")
	owner, sig := q.Get("owner"), q.Get("sig")
	expires, err := strconv.ParseInt(expiration, 10, 64)
	want := imageMAC(m.cfg.Secret, owner+"\x00"+id+"\x00"+name+"\x00"+expiration)
	now := time.Now().Unix()
	if err != nil || expires < now || expires > now+int64(sessionRetention/time.Second)+60 || len(owner) != 64 || !safeNativeSessionID(id) || !strings.HasPrefix(name, "portal_image_") || !strings.HasSuffix(name, ".png") || !validImageStem(strings.TrimSuffix(name, ".png")) || !hmac.Equal([]byte(sig), []byte(want)) {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(m.cfg.Home)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	brain, err := root.OpenRoot(filepath.Join("agy", "brain", id))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer brain.Close()
	f, err := brain.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// AGY may stream Markdown links such as ![image](part_0). Replace its internal
// part names across chunk boundaries with the real, scoped image URL.
func rewriteImageParts(text string, refs []string, final bool) (string, string) {
	var out strings.Builder
	for len(text) > 0 {
		i := strings.Index(text, "part_")
		if i < 0 {
			if !final {
				for n := min(4, len(text)); n > 0; n-- {
					if strings.HasSuffix(text, "part_"[:n]) {
						out.WriteString(text[:len(text)-n])
						return out.String(), text[len(text)-n:]
					}
				}
			}
			out.WriteString(text)
			break
		}
		out.WriteString(text[:i])
		text = text[i:]
		end := 5
		for end < len(text) && text[end] >= '0' && text[end] <= '9' {
			end++
		}
		if end == len(text) && !final {
			return out.String(), text
		}
		n, err := strconv.Atoi(text[5:end])
		if err == nil && n >= 0 && n < len(refs) && refs[n] != "" {
			out.WriteString(refs[n])
		} else {
			out.WriteString(text[:end])
		}
		text = text[end:]
	}
	return out.String(), ""
}

func (o *response) text(v string, reasoning bool) error {
	if reasoning || len(o.imageRefs) == 0 {
		return o.writeText(v, reasoning)
	}
	v, o.imagePending = rewriteImageParts(o.imagePending+v, o.imageRefs, false)
	if v == "" {
		return nil
	}
	return o.writeText(v, false)
}

func (o *response) finishImages() error {
	for i, link := range o.imageLinks {
		if i < len(o.imageRefs) && !strings.Contains(o.textValue, o.imageRefs[i]) {
			if err := o.writeText("\n\n![生成的图片]("+o.imageRefs[i]+")", false); err != nil {
				return err
			}
		}
		if link != "" && !strings.Contains(o.textValue, link) {
			if err := o.writeText("\n\n[下载原图]("+link+")", false); err != nil {
				return err
			}
		}
	}
	return nil
}
