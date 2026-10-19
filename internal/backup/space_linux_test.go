//go:build linux

package backup

import (
	"math"
	"strings"
	"testing"
)

func TestRequireSpaceReportsNeededAndAvailableWithoutServiceAssumptions(t *testing.T) {
	dir := t.TempDir()
	if err := requireSpace(dir, 0); err != nil {
		t.Fatalf("zero additional space rejected: %v", err)
	}
	err := requireSpace(dir, math.MaxInt64)
	if err == nil || !strings.Contains(err.Error(), "至少需要") || !strings.Contains(err.Error(), "当前仅") || strings.Contains(err.Error(), "未停止服务") {
		t.Fatalf("unhelpful or misleading space error: %v", err)
	}
	if err := requireSpace(dir, -1); err == nil {
		t.Fatal("negative requirement accepted")
	}
}
