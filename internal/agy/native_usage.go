package agy

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The official CLI stores completed model generations as step_type=15,
// status=3, with ModelUsageStats in metadata field 9. Read only new usage
// metadata; request outcomes come from ACP, never native error/history payloads.
type nativeUsageReader struct{ db *sql.DB }

func openNativeUsage(home, id string) (*nativeUsageReader, error) {
	if !safeNativeSessionID(id) {
		return nil, errors.New("invalid native session id")
	}
	path := filepath.Join(home, "agy", "conversations", id+".db")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("invalid native usage database")
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(100)", "query_only(1)", "cache_size(-256)", "mmap_size(0)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &nativeUsageReader{db: db}, nil
}
func nativeUsageCursor(ctx context.Context, home, id string) (int64, error) {
	r, err := openNativeUsage(home, id)
	if err != nil {
		return 0, err
	}
	defer r.db.Close()
	c, done := context.WithTimeout(ctx, 250*time.Millisecond)
	defer done()
	var idx int64
	err = r.db.QueryRowContext(c, `SELECT COALESCE(MAX(idx),-1) FROM steps`).Scan(&idx)
	return idx, err
}

type nativeTokenCounts struct{ input, output, cacheRead, cacheWrite, thought int64 }

func walkUsageProto(b []byte, visit func(int, uint64, []byte) error) error {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 || tag>>3 > math.MaxInt32 {
			return errors.New("invalid usage protobuf tag")
		}
		b = b[n:]
		field := int(tag >> 3)
		switch tag & 7 {
		case 0:
			value, n := binary.Uvarint(b)
			if n <= 0 {
				return errors.New("invalid usage integer")
			}
			b = b[n:]
			if err := visit(field, value, nil); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return errors.New("short usage fixed64")
			}
			b = b[8:]
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-max(n, 0)) {
				return errors.New("invalid usage bytes")
			}
			b = b[n:]
			payload := b[:int(size)]
			b = b[int(size):]
			if err := visit(field, 0, payload); err != nil {
				return err
			}
		case 5:
			if len(b) < 4 {
				return errors.New("short usage fixed32")
			}
			b = b[4:]
		default:
			return errors.New("unsupported usage wire type")
		}
	}
	return nil
}

func decodeNativeUsage(b []byte) (Usage, error) {
	var counts nativeTokenCounts
	found, inputPresent, outputPresent := false, false, false
	err := walkUsageProto(b, func(field int, _ uint64, payload []byte) error {
		if field != 9 || payload == nil {
			return nil
		}
		if found {
			return errors.New("duplicate model usage stats")
		}
		found = true
		return walkUsageProto(payload, func(field int, value uint64, bytes []byte) error {
			if bytes != nil {
				return nil
			}
			if field != 2 && field != 3 && field != 4 && field != 5 && field != 9 {
				return nil
			}
			if value > math.MaxInt64 {
				return errors.New("invalid token count")
			}
			switch field {
			case 2:
				counts.input = int64(value)
				inputPresent = true
			case 3:
				counts.output = int64(value)
				outputPresent = true
			case 4:
				counts.cacheWrite = int64(value)
			case 5:
				counts.cacheRead = int64(value)
			case 9:
				counts.thought = int64(value)
			}
			return nil
		})
	})
	if err != nil {
		return Usage{}, err
	}
	// Proto3 omits legitimate zero fields. At least input or output must exist;
	// an empty submessage is not evidence of a measured zero-cost generation.
	if !found || !(inputPresent || outputPresent) {
		return Usage{}, errors.New("model usage missing")
	}
	if counts.thought > counts.output {
		return Usage{}, errors.New("reasoning exceeds inclusive output")
	}
	if counts.cacheRead > math.MaxInt64-counts.cacheWrite {
		return Usage{}, errors.New("token overflow")
	}
	cache := counts.cacheRead + counts.cacheWrite
	if counts.input > math.MaxInt64-cache {
		return Usage{}, errors.New("token overflow")
	}
	input := counts.input + cache
	if input > math.MaxInt64-counts.output {
		return Usage{}, errors.New("token overflow")
	}
	// Responses input includes cache, output includes reasoning. They are detail
	// subsets, so total must not add either category a second time.
	return Usage{Known: true, Input: input, Output: counts.output, Cache: cache, Reasoning: counts.thought, Total: input + counts.output}, nil
}

func (r *nativeUsageReader) after(ctx context.Context, after int64) (Usage, int64, int, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT idx,metadata FROM steps WHERE idx>? AND step_type=15 AND status=3 ORDER BY idx LIMIT 2049`, after)
	if err != nil {
		return Usage{}, after, 0, err
	}
	defer rows.Close()
	var total Usage
	cursor, calls := after, 0
	for rows.Next() {
		var idx int64
		var metadata []byte
		if err = rows.Scan(&idx, &metadata); err != nil {
			return Usage{}, after, 0, err
		}
		if len(metadata) > 1<<20 || calls >= 2048 {
			return Usage{}, after, 0, errors.New("usage batch exceeds bound")
		}
		v, e := decodeNativeUsage(metadata)
		if e != nil {
			return Usage{}, after, 0, e
		}
		if total.Total > math.MaxInt64-v.Total {
			return Usage{}, after, 0, errors.New("usage batch overflow")
		}
		total.Input += v.Input
		total.Output += v.Output
		total.Cache += v.Cache
		total.Reasoning += v.Reasoning
		total.Total += v.Total
		total.Known = true
		cursor = idx
		calls++
	}
	if err := rows.Err(); err != nil {
		return Usage{}, after, 0, err
	}
	return total, cursor, calls, nil
}

func (m *Manager) startUsage(s *session) {
	idx, err := nativeUsageCursor(s.life, m.cfg.Home, s.ID)
	if err != nil {
		idx = -2
	}
	s.mu.Lock()
	s.usageCursor = idx
	s.mu.Unlock()
}
func (m *Manager) consumeUsage(s *session) Usage {
	s.mu.Lock()
	cursor := s.usageCursor
	s.mu.Unlock()
	if cursor < -1 {
		return Usage{}
	}
	r, err := openNativeUsage(m.cfg.Home, s.ID)
	if err != nil {
		return Usage{}
	}
	defer r.db.Close()
	ctx, done := context.WithTimeout(s.life, 350*time.Millisecond)
	defer done()
	for attempt := 0; attempt < 4; attempt++ {
		u, next, calls, err := r.after(ctx, cursor)
		if err != nil {
			return Usage{}
		}
		if calls > 0 {
			s.mu.Lock()
			s.usageCursor = next
			s.mu.Unlock()
			return u
		}
		if attempt < 3 {
			select {
			case <-time.After(40 * time.Millisecond):
			case <-ctx.Done():
				return Usage{}
			}
		}
	}
	return Usage{}
}
