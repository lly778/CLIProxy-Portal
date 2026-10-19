package gateway

import (
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/resourcecontrol"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"errors"
	"github.com/klauspost/compress/zstd"
	"io"
	"net/http"
	"strings"
)

type resourceRequestKey struct{}
type resourceRequest struct {
	lease       *resourcecontrol.Lease
	enforce     bool
	limit       int64
	transferred bool
}

func resourceInference(r *http.Request) bool {
	return isDialoguePath(r.Method, r.URL.Path) || (r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/v1beta/") || strings.HasPrefix(r.URL.Path, "/backend-api/"))) || (r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"))
}
func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if g.Resources == nil || !resourceInference(r) {
		g.serveProtectedHTTP(w, r)
		return
	}
	user := "unassigned"
	key := apiKey(r)
	if key != "" {
		if owner, err := g.store.KeyByHash(r.Context(), cpamp.HashAPIKey(key)); err == nil && owner.UserID != "" {
			user = owner.UserID
		}
	}
	lease, reason := g.Resources.AcquireRequest(user, r.ContentLength)
	if lease == nil {
		if reason == "request_too_large" {
			resourceError(w, 413, reason, "请求过大，请压缩图片或减少历史消息后重试")
			return
		}
		status := 429
		if reason != "global_concurrency" && reason != "user_concurrency" {
			status = 503
		}
		resourceError(w, status, reason, "服务繁忙，请稍后重试")
		return
	}
	p := lease.Policy
	limit := p.BodyMiB * resourcecontrol.MiB
	state := &resourceRequest{lease: lease, enforce: p.Mode == "enforce", limit: limit}
	defer lease.ModelDone()
	defer func() {
		if !state.transferred {
			lease.Finish()
		}
	}()
	if state.enforce && limit > 0 && r.Method == http.MethodPost {
		cleanup, code, err := g.prepareResourceBody(r, limit)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			lease.RecordStatus("request_too_large_or_invalid", code)
			message := "请求内容无效或压缩格式不支持"
			if code == 413 {
				message = "请求过大，请压缩图片或减少历史消息后重试"
			}
			resourceError(w, code, "request_body_rejected", message)
			return
		}
	} else if !state.enforce && limit > 0 && r.Body != nil && r.ContentLength <= limit {
		r.Body = &observedResourceBody{ReadCloser: r.Body, limit: limit, onLarge: func() { lease.Record("request_too_large") }}
	}
	if state.enforce && limit > 0 && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		r.Header.Del("Sec-WebSocket-Extensions")
	}
	r = r.WithContext(context.WithValue(r.Context(), resourceRequestKey{}, state))
	g.serveProtectedHTTP(w, r)
}

func resourceError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status == 429 || status == 503 {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "resource_limit", "code": code, "message": message}})
}

type observedResourceBody struct {
	io.ReadCloser
	limit, read int64
	reported    bool
	onLarge     func()
}

func (b *observedResourceBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	b.read += int64(n)
	if !b.reported && b.read > b.limit {
		b.reported = true
		b.onLarge()
	}
	return n, e
}

// Prevalidate using private, unlinked disk files. No oversized or compressed
// payload reaches CPA before its wire and decoded sizes have both been checked.
func (g *Gateway) prepareResourceBody(r *http.Request, limit int64) (func(), int, error) {
	spools := g.identitySpools()
	cleanup := spools.close
	wire, err := spools.create()
	if err != nil {
		return cleanup, 503, err
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	n, err := identityCopy(r.Context(), wire, io.LimitReader(r.Body, limit+1))
	_ = r.Body.Close()
	if err != nil {
		return cleanup, 400, err
	}
	if n > limit {
		return cleanup, 413, errors.New("wire limit")
	}
	current, size := wire, n
	encodings := strings.Split(r.Header.Get("Content-Encoding"), ",")
	if len(encodings) > 3 {
		return cleanup, 415, errors.New("too many encodings")
	}
	for i := len(encodings) - 1; i >= 0; i-- {
		encoding := strings.ToLower(strings.TrimSpace(encodings[i]))
		if encoding == "" || encoding == "identity" {
			continue
		}
		var reader io.ReadCloser
		section := io.NewSectionReader(current, 0, size)
		switch encoding {
		case "gzip", "x-gzip":
			reader, err = gzip.NewReader(section)
		case "deflate":
			reader, err = zlib.NewReader(section)
			if err != nil {
				reader = flate.NewReader(io.NewSectionReader(current, 0, size))
				err = nil
			}
		case "zstd":
			var z *zstd.Decoder
			z, err = zstd.NewReader(section, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxMemory(32<<20))
			if err == nil {
				reader = z.IOReadCloser()
			}
		default:
			return cleanup, 415, errors.New("unsupported encoding")
		}
		if err != nil {
			return cleanup, 400, err
		}
		decoded, e := spools.create()
		if e != nil {
			_ = reader.Close()
			return cleanup, 503, e
		}
		decodedSize, e := identityCopy(r.Context(), decoded, io.LimitReader(reader, limit+1))
		_ = reader.Close()
		if e != nil {
			return cleanup, 400, e
		}
		if decodedSize > limit {
			return cleanup, 413, errors.New("decoded limit")
		}
		if current != wire {
			_ = current.Close()
		}
		current, size = decoded, decodedSize
	}
	r.Body = io.NopCloser(io.NewSectionReader(wire, 0, n))
	r.ContentLength = n
	r.TransferEncoding = nil
	return cleanup, 0, nil
}
