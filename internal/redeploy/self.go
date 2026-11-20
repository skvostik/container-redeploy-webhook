package redeploy

import (
	"bufio"
	"io"
	"os"
	"regexp"
)

var containerIDInMount = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// SelfContainerID returns the ID of the container this process runs in, or
// "" when it cannot be determined (e.g. not running in Docker).
//
// Docker bind-mounts /etc/hostname, /etc/hosts and /etc/resolv.conf from
// /var/lib/docker/containers/<id>/, which shows up in /proc/self/mountinfo.
func SelfContainerID() string {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	return selfIDFromMountinfo(f)
}

func selfIDFromMountinfo(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if m := containerIDInMount.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}
