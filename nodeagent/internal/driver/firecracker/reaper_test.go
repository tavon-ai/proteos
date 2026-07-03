//go:build firecracker && linux

package firecracker

import (
	"testing"
	"time"
)

func TestHibernateTimeout(t *testing.T) {
	tests := []struct {
		memMiB int
		want   time.Duration
	}{
		{256, hibernateGraceBase + 1*hibernateGracePerGiB},  // 256 MiB rounds up to 1 GiB
		{1024, hibernateGraceBase + 1*hibernateGracePerGiB}, // exactly 1 GiB
		{1025, hibernateGraceBase + 2*hibernateGracePerGiB}, // 1 MiB over → 2 GiB
		{4096, hibernateGraceBase + 4*hibernateGracePerGiB}, // 4 GiB
		{8192, hibernateGraceBase + 8*hibernateGracePerGiB}, // 8 GiB
	}
	for _, tc := range tests {
		got := hibernateTimeout(tc.memMiB)
		if got != tc.want {
			t.Errorf("hibernateTimeout(%d MiB) = %v, want %v", tc.memMiB, got, tc.want)
		}
	}
}
