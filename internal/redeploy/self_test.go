package redeploy

import (
	"strings"
	"testing"
)

func TestSelfIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	info := `1 0 0:1 / / rw - overlay overlay rw
2 1 8:1 /var/lib/docker/containers/` + id + `/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw
`
	if got := selfIDFromMountinfo(strings.NewReader(info)); got != id {
		t.Fatalf("got %q", got)
	}
	if got := selfIDFromMountinfo(strings.NewReader("1 0 0:1 / / rw - ext4 /dev/sda1 rw\n")); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
