package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

const modelListLimit = 2 << 20

type Gateway struct {
	upstream     *url.URL
	keys         *service.Keys
	store        *store.Store
	vault        *Vault
	logger       *slog.Logger
	captureSlots chan struct{}
}

func New(upstreamURL string, keys *service.Keys, st *store.Store, vault *Vault, logger *slog.Logger) (*Gateway, error) {
	upstream, err := url.Parse(upstreamURL)
	if err != nil || upstream == nil || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.Host == "" || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		return nil, errors.New("CPA_UPSTREAM_URL must be a plain http(s) URL")
	}
	if keys == nil || st == nil || vault == nil {
		return nil, errors.New("gateway dependencies are incomplete")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{upstream: upstream, keys: keys, store: st, vault: vault, logger: logger, captureSlots: make(chan struct{}, 1)}, nil
}

func (g *Gateway) Handler() http.Handler {
	return http.HandlerFunc(g.serveHTTP)
}

func allowedPath(path string) bool {
	for _, prefix := range []string{"/v1/", "/v1beta/", "/openai/v1/", "/backend-api/codex/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowedPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	isModelList := r.Method == http.MethodGet && r.URL.Path == "/v1/models"
	var capture *requestCapture
	if isDialoguePath(r.Method, r.URL.Path) {
		capture = g.startCapture(r)
		if capture != nil {
			defer g.finishCapture(capture)
			if r.Body != nil {
				r.Body = &teeReadCloser{Reader: io.TeeReader(r.Body, capture.request), Closer: r.Body}
			}
		}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.SetURL(g.upstream)
			out.SetXForwarded()
			// Browser sessions and proxy credentials are not CPA API credentials.
			out.Out.Header.Del("Cookie")
			out.Out.Header.Del("Proxy-Authorization")
			if isModelList || capture != nil {
				out.Out.Header.Set("Accept-Encoding", "identity")
			}
			if isModelList {
				out.Out.Header.Del("If-None-Match")
				out.Out.Header.Del("If-Modified-Since")
			}
		},
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Set-Cookie")
			if isModelList && resp.StatusCode == http.StatusOK {
				return g.filterModelList(resp)
			}
			if capture != nil {
				capture.status = resp.StatusCode
				capture.responseContentType = resp.Header.Get("Content-Type")
				capture.cpaRequestID = cpaRequestID(resp.Header.Get("X-CPA-TRACE-ID"))
				resp.Body = &teeReadCloser{Reader: io.TeeReader(resp.Body, capture.response), Closer: resp.Body}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// Transport errors may embed full URLs (including query credentials).
			g.logger.Warn("gateway upstream request failed", "path", r.URL.Path, "error_type", fmt.Sprintf("%T", err))
			if capture != nil {
				capture.status = http.StatusBadGateway
				capture.responseContentType = "text/plain; charset=utf-8"
				_, _ = capture.response.Write([]byte("Bad Gateway\n"))
			}
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

func isDialoguePath(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	for _, suffix := range []string{"/responses", "/chat/completions", "/messages"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	if strings.HasPrefix(path, "/v1beta/models/") && (strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")) {
		return true
	}
	return false
}

type teeReadCloser struct {
	io.Reader
	io.Closer
}

type requestCapture struct {
	row                 store.GatewayCapture
	request             *CaptureWriter
	response            *CaptureWriter
	status              int
	responseContentType string
	cpaRequestID        string
}

func apiKey(r *http.Request) string {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}
	for _, name := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		if key := strings.TrimSpace(r.Header.Get(name)); key != "" {
			return key
		}
	}
	return ""
}

func newCaptureID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func (g *Gateway) startCapture(r *http.Request) *requestCapture {
	key := apiKey(r)
	if key == "" {
		return nil
	}
	hash := cpamp.HashAPIKey(key)
	owner, err := g.store.KeyByHash(r.Context(), hash)
	if err != nil || owner.UserID == "" {
		return nil
	}
	id, err := newCaptureID()
	if err != nil {
		return nil
	}
	request, err := g.vault.Create(id, "rawrequest")
	if err != nil {
		g.logger.Warn("gateway request capture unavailable", "error", err)
		return nil
	}
	response, err := g.vault.Create(id, "rawresponse")
	if err != nil {
		_, _ = request.Close()
		g.vault.Delete(id)
		g.logger.Warn("gateway response capture unavailable", "error", err)
		return nil
	}
	return &requestCapture{
		row:     store.GatewayCapture{ID: id, UserID: owner.UserID, APIKeyHash: hash, CreatedAt: time.Now().UTC(), Method: r.Method, Path: r.URL.Path, RequestContentType: r.Header.Get("Content-Type")},
		request: request, response: response,
	}
}

func (g *Gateway) finishCapture(capture *requestCapture) {
	requestTruncated, requestErr := capture.request.Close()
	responseTruncated, responseErr := capture.response.Close()
	if requestErr != nil || responseErr != nil {
		g.vault.Delete(capture.row.ID)
		g.logger.Warn("gateway capture incomplete", "request_error", requestErr, "response_error", responseErr)
		return
	}
	g.captureSlots <- struct{}{}
	defer func() { <-g.captureSlots }()
	g.vault.LockSharedMessages()
	defer g.vault.UnlockSharedMessages()
	capture.row.RequestTruncated = requestTruncated
	capture.row.ResponseTruncated = responseTruncated
	capture.row.StatusCode = capture.status
	capture.row.ResponseContentType = capture.responseContentType
	capture.row.CPARequestID = capture.cpaRequestID
	requestRaw, err := g.vault.Read(capture.row.ID, "rawrequest")
	if err != nil {
		g.vault.Delete(capture.row.ID)
		g.logger.Warn("gateway request dialogue extraction failed", "error", err)
		return
	}
	responseRaw, err := g.vault.Read(capture.row.ID, "rawresponse")
	if err != nil {
		g.vault.Delete(capture.row.ID)
		g.logger.Warn("gateway response dialogue extraction failed", "error", err)
		return
	}
	var payload struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(requestRaw, &payload) == nil {
		capture.row.RequestedModel = payload.Model
	}
	requestMessages, requestTextTruncated := limitRecordEvents(requestRecordEvents(requestRaw), CaptureLimit)
	responseMessages, responseTextTruncated := limitRecordEvents(responseRecordEvents(responseRaw, capture.responseContentType), CaptureLimit)
	// Raw wire data is a temporary encrypted parsing input, never a log entry.
	for _, kind := range []string{"rawrequest", "rawresponse"} {
		if path, err := g.vault.filePath(capture.row.ID, kind); err == nil {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				g.vault.Delete(capture.row.ID)
				g.logger.Warn("gateway raw capture removal failed", "error", err)
				return
			}
		}
	}
	if len(requestMessages) == 0 && len(responseMessages) == 0 {
		return
	}
	// Empty encrypted markers identify the two sides of an indexed capture;
	// actual events are stored as shared encrypted records.
	for _, kind := range []string{"request", "response"} {
		if _, err := g.saveDialogue(capture.row.ID, kind, ""); err != nil {
			g.vault.Delete(capture.row.ID)
			g.logger.Warn("gateway dialogue marker save failed", "side", kind, "error", err)
			return
		}
	}
	capture.row.RequestContentType = StructuredCaptureContentType
	capture.row.ResponseContentType = StructuredCaptureContentType
	capture.row.RequestTruncated = capture.row.RequestTruncated || requestTextTruncated
	capture.row.ResponseTruncated = capture.row.ResponseTruncated || responseTextTruncated
	var references []store.GatewayCaptureMessage
	var newlyCreated []string
	for _, side := range []struct {
		name     string
		messages []RecordEvent
	}{{"request", requestMessages}, {"response", responseMessages}} {
		for _, message := range side.messages {
			encoded, err := json.Marshal(message)
			if err != nil {
				g.vault.Delete(capture.row.ID)
				for _, id := range newlyCreated {
					g.vault.DeleteSharedMessage(id)
				}
				g.logger.Warn("gateway structured event encoding failed", "error", err)
				return
			}
			id, created, err := g.vault.SaveSharedMessage(capture.row.UserID, StructuredMessageRole, string(encoded))
			if err != nil {
				g.vault.Delete(capture.row.ID)
				for _, id := range newlyCreated {
					g.vault.DeleteSharedMessage(id)
				}
				g.logger.Warn("gateway shared dialogue save failed", "error", err)
				return
			}
			if created {
				newlyCreated = append(newlyCreated, id)
			}
			references = append(references, store.GatewayCaptureMessage{Side: side.name, ID: id, Role: StructuredMessageRole})
		}
	}
	// The client may have disconnected, but the capture must still be indexed.
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.store.SaveGatewayCaptureWithMessages(saveCtx, capture.row, references); err != nil {
		g.vault.Delete(capture.row.ID)
		for _, id := range newlyCreated {
			g.vault.DeleteSharedMessage(id)
		}
		g.logger.Warn("gateway capture index failed", "error", err)
		return
	}
	deleted, err := g.store.DeleteExcessGatewayCaptures(saveCtx, capture.row.UserID, 100)
	if err != nil {
		g.logger.Warn("gateway capture cleanup failed", "error", err)
		return
	}
	for _, id := range deleted.CaptureIDs {
		g.vault.Delete(id)
	}
	for _, id := range deleted.MessageIDs {
		g.vault.DeleteSharedMessage(id)
	}
}

func (g *Gateway) saveDialogue(id, kind, value string) (bool, error) {
	writer, err := g.vault.Create(id, kind)
	if err != nil {
		return false, err
	}
	_, _ = writer.Write([]byte(value))
	return writer.Close()
}

func cpaRequestID(traceID string) string {
	part := traceID
	if index := strings.LastIndexByte(traceID, '-'); index >= 0 {
		part = traceID[index+1:]
	}
	if len(part) != 8 {
		return ""
	}
	for _, ch := range part {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return ""
		}
	}
	return part
}

func (g *Gateway) filterModelList(resp *http.Response) error {
	if resp.Header.Get("Content-Encoding") != "" && resp.Header.Get("Content-Encoding") != "identity" {
		return errors.New("compressed model list cannot be filtered")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, modelListLimit+1))
	if err != nil || len(data) > modelListLimit {
		return errors.New("model list exceeds filtering limit")
	}
	hidden, err := g.keys.HiddenModelAliases(resp.Request.Context())
	if err != nil {
		return fmt.Errorf("read visible models: %w", err)
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Data == nil {
		return errors.New("invalid upstream model list")
	}
	filtered := make([]json.RawMessage, 0, len(payload.Data))
	for _, item := range payload.Data {
		var model struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(item, &model) != nil || model.ID == "" {
			return errors.New("invalid upstream model entry")
		}
		if !hidden[strings.ToLower(model.ID)] {
			filtered = append(filtered, item)
		}
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return err
	}
	envelope["data"] = encoded
	data, err = json.Marshal(envelope)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Set("Content-Length", fmt.Sprint(len(data)))
	resp.Header.Set("Cache-Control", "no-store")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("ETag")
	resp.Header.Del("Content-MD5")
	return nil
}
