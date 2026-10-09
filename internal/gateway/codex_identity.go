package gateway

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var identityOpening = regexp.MustCompile(`(?i)^[\s\p{Zs}\x{FEFF}]*(?:you[\s\p{Zs}]+are|you['’]re|i[\s\p{Zs}]+am|i['’]m|(?:codex|this[\s\p{Zs}]+(?:assistant|agent|model)|the[\s\p{Zs}]+(?:assistant|agent|model))[\s\p{Zs}]+is)[\s\p{Zs}]+`)
var identityQualifier = regexp.MustCompile(`(?i)[\s\p{Zs}]+based[\s\p{Zs}]+on[\s\p{Zs}]+GPT-5`)

func (g *Gateway) applyCodexIdentityCompatibility(r *http.Request) (func(), error) {
	if r.Body == nil || r.Body == http.NoBody || !isDialoguePath(r.Method, r.URL.Path) {
		return nil, nil
	}
	enabled, err := g.store.EnabledCodexIdentityChannels(r.Context())
	if err != nil || len(enabled) == 0 {
		return nil, nil
	}
	encoding := r.Header.Get("Content-Encoding")
	for _, value := range strings.Split(encoding, ",") {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "", "identity", "gzip", "x-gzip", "deflate":
		default:
			return nil, nil
		}
	}
	spools := g.identitySpools()
	cleanup := spools.close
	wire, err := spools.create()
	if err != nil {
		return cleanup, err
	}
	size, err := identityCopy(r.Context(), wire, r.Body)
	if err != nil {
		return cleanup, err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(io.NewSectionReader(wire, 0, size))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(io.NewSectionReader(wire, 0, size)), nil }

	decoded, decodedSize, err := decodeIdentitySpool(r.Context(), spools, wire, size, encoding)
	if err != nil {
		if r.Context().Err() != nil {
			return cleanup, r.Context().Err()
		}
		spools.keep(wire)
		return cleanup, nil
	}
	plan, err := scanIdentityJSON(r.Context(), spools, decoded, decodedSize, r.URL.Path)
	if err != nil {
		if !errors.Is(err, errIdentityJSON) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return cleanup, err
		}
		spools.keep(wire)
		return cleanup, nil
	}
	if plan.patchCount == 0 {
		spools.keep(wire)
		return cleanup, nil
	}
	allowed, err := g.keys.ShouldRemoveGPT5Identity(r.Context(), plan.model, enabled)
	if err != nil {
		g.logger.Warn("Codex identity compatibility routing unavailable", "path", r.URL.Path)
		spools.keep(wire)
		return cleanup, nil
	}
	if !allowed {
		spools.keep(wire)
		return cleanup, nil
	}
	modified, modifiedSize, err := applyIdentityPatches(r.Context(), spools, decoded, decodedSize, plan)
	if err != nil {
		return cleanup, err
	}
	r.Body = io.NopCloser(io.NewSectionReader(modified, 0, modifiedSize))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(modified, 0, modifiedSize)), nil
	}
	r.ContentLength = modifiedSize
	r.TransferEncoding = nil
	r.Header.Set("Content-Length", strconv.FormatInt(modifiedSize, 10))
	for _, name := range []string{"Content-Encoding", "Content-MD5", "Digest", "Content-Digest", "Transfer-Encoding"} {
		r.Header.Del(name)
	}
	spools.keep(modified)
	g.logger.Info("Codex GPT-5 identity description removed", "model", plan.model, "path", r.URL.Path)
	return cleanup, nil
}
