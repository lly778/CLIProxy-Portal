package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func withoutGPT5Identity(text string) (string, bool) {
	var spans []identitySpan
	changed, err := scanIdentityText(identityTextSource{strings.NewReader(text), identitySpan{0, int64(len(text))}, false}, func(span identitySpan) error {
		spans = append(spans, span)
		return nil
	})
	if err != nil || !changed {
		return text, false
	}
	var out strings.Builder
	var cursor int64
	for _, span := range spans {
		out.WriteString(text[cursor:span.start])
		cursor = span.end
	}
	out.WriteString(text[cursor:])
	return out.String(), true
}

func rewriteIdentityPayloadForTest(t *testing.T, raw []byte, path string) ([]byte, string, bool) {
	t.Helper()
	spools := &identitySpools{dir: t.TempDir()}
	defer spools.close()
	file, err := spools.create()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(raw); err != nil {
		t.Fatal(err)
	}
	plan, err := scanIdentityJSON(t.Context(), spools, file, int64(len(raw)), path)
	if err != nil {
		return raw, "", false
	}
	if plan.patchCount == 0 {
		return raw, plan.model, false
	}
	out, size, err := applyIdentityPatches(t.Context(), spools, file, int64(len(raw)), plan)
	if err != nil {
		t.Fatal(err)
	}
	result, err := io.ReadAll(io.NewSectionReader(out, 0, size))
	if err != nil {
		t.Fatal(err)
	}
	return result, plan.model, true
}

func TestIdentityStreamingPreservesEscapesOrderingAndUnknownFields(t *testing.T) {
	raw := []byte(`{"input":[{"content":[{"text":"You are \u0043odex, \ud83d\ude80 an assistant based\u0020on\tGPT-5.\nRules.","type":"input_text"}],"type":"message","role":"developer"},{"content":"You are an assistant based on GPT-5.","role":"user"}],"instructions":"You are an assistant based on GPT-5.","opaque":{"integer":9007199254740993,"number":1e999,"null":null,"bool":true},"model":"gemini-real"}`)
	got, model, changed := rewriteIdentityPayloadForTest(t, raw, "/v1/responses")
	want := bytes.Replace(raw, []byte(` based\u0020on\tGPT-5`), nil, 1)
	want = bytes.Replace(want, []byte(`"instructions":"You are an assistant based on GPT-5."`), []byte(`"instructions":"You are an assistant."`), 1)
	if !changed || model != "gemini-real" || !bytes.Equal(got, want) {
		t.Fatalf("effective JSON changed outside removal spans:\n%s", got)
	}
	for _, bad := range []string{
		`{"instructions":"` + oldIdentity + `","model":"gemini-real",}`,
		`{"instructions":"` + oldIdentity + `","opaque":[true,]}`,
		`{"instructions":"` + oldIdentity + `","opaque":01}`,
		`{"instructions":"` + oldIdentity + `","opaque":1.}`,
		`{"instructions":"` + oldIdentity + `","opaque":1e+}`,
		`{"instructions":"` + oldIdentity + `","instructions":"another"}`,
		`{"input":[{"role":"developer","content":"` + oldIdentity + `","role":"user"}]}`,
		`{"instructions":"` + oldIdentity + `","opaque":"\q"}`,
		`{"instructions":"` + oldIdentity + `"} trailing`,
	} {
		got, _, changed := rewriteIdentityPayloadForTest(t, []byte(bad), "/v1/responses")
		if changed || string(got) != bad {
			t.Fatalf("malformed/ambiguous JSON was changed: %s", got)
		}
	}
}

type identityRepeatReader struct{ block []byte }

func (r identityRepeatReader) Read(p []byte) (int, error) {
	for written := 0; written < len(p); {
		written += copy(p[written:], r.block)
	}
	return len(p), nil
}

func TestIdentityStreamingLargePayloadUsesBoundedMemory(t *testing.T) {
	spools := &identitySpools{dir: t.TempDir()}
	defer spools.close()
	file, err := spools.create()
	if err != nil {
		t.Fatal(err)
	}
	const padding = int64(64 << 20)
	prefix := `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"`
	tail := `"}]}],"instructions":"` + oldIdentity + `","model":"gemini-real"}`
	if _, err := io.WriteString(file, prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(file, identityRepeatReader{[]byte("abcdefgh")}, padding); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, tail); err != nil {
		t.Fatal(err)
	}
	size := int64(len(prefix)) + padding + int64(len(tail))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	plan, err := scanIdentityJSON(t.Context(), spools, file, size, "/v1/responses")
	if err != nil || plan == nil || plan.patchCount != 1 {
		t.Fatalf("large payload not handled: %+v %v", plan, err)
	}
	out, resultSize, err := applyIdentityPatches(t.Context(), spools, file, size, plan)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("64 MiB request allocated %d bytes while rewriting", allocated)
	}
	removed := int64(len(" based on GPT-5"))
	if resultSize != size-removed {
		t.Fatalf("unexpected size: %d", resultSize)
	}
	actual := sha256.New()
	_, _ = io.Copy(actual, io.NewSectionReader(out, 0, resultSize))
	expected := sha256.New()
	_, _ = io.WriteString(expected, prefix)
	_, _ = io.CopyN(expected, identityRepeatReader{[]byte("abcdefgh")}, padding)
	_, _ = io.WriteString(expected, strings.Replace(tail, " based on GPT-5", "", 1))
	if hex.EncodeToString(actual.Sum(nil)) != hex.EncodeToString(expected.Sum(nil)) {
		t.Fatal("large image or trailing fields changed")
	}
}

func TestIdentitySpoolsCancellationAndCleanup(t *testing.T) {
	dir := t.TempDir()
	spools := &identitySpools{dir: dir}
	file, err := spools.create()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := identityCopy(ctx, file, strings.NewReader("private")); err == nil {
		t.Fatal("canceled copy succeeded")
	}
	spools.close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary body files survived: %v %v", entries, err)
	}
}
