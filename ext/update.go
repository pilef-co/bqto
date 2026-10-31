package ext

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type Release struct {
	Version  string
	Newer    bool
	Security bool
	Ref      string
}

type Updater struct {
	Name     string
	Line     func() string
	Current  func() string
	Timeout  time.Duration
	Latest   func(ctx context.Context) (Release, error)
	Download func(ctx context.Context, r Release, dir string) (archive string, err error)
	DirFix   string
}

var updater *Updater

func SetUpdater(u Updater) {
	mu.Lock()
	defer mu.Unlock()
	updater = &u
}

func CustomUpdater() (Updater, bool) {
	mu.RLock()
	defer mu.RUnlock()
	if updater == nil {
		return Updater{}, false
	}
	return *updater, true
}

func ParseVersion(v string) ([3]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	end := strings.IndexAny(v, "-+")
	if end >= 0 {
		v = v[:end]
	}
	parts := strings.Split(v, ".")
	var out [3]int
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p[0] < '0' || p[0] > '9' {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func NewerVersion(latest, have string) bool {
	l, okL := ParseVersion(latest)
	h, okH := ParseVersion(have)
	if !okL || !okH {
		return false
	}
	for i := range l {
		if l[i] != h[i] {
			return l[i] > h[i]
		}
	}
	return preRelease(have) && !preRelease(latest)
}

func preRelease(v string) bool {
	core, _, _ := strings.Cut(v, "+")
	return strings.Contains(core, "-")
}

func ShortVersion(v string) string {
	n, ok := ParseVersion(v)
	if !ok {
		return v
	}
	return strconv.Itoa(n[0]) + "." + strconv.Itoa(n[1]) + "." + strconv.Itoa(n[2])
}
