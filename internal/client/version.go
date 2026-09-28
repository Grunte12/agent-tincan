package client

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

var releaseVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var developmentVersion = regexp.MustCompile(`(^|[.-])(dev|dirty)([.-]|$)|-[0-9]+-g[0-9a-f]+`)

// Newer reports whether release a is newer than b. Invalid and development
// builds are skipped, as are prerelease clients unless a is a prerelease.
func Newer(a, b string) bool {
	av, bv := releaseVersion.FindStringSubmatch(a), releaseVersion.FindStringSubmatch(b)
	if av == nil || bv == nil || developmentVersion.MatchString(a) || developmentVersion.MatchString(b) {
		return false
	}
	for _, v := range [][]string{av, bv} {
		for id := range strings.SplitSeq(v[4], ".") {
			if numeric(id) && len(id) > 1 && id[0] == '0' {
				return false
			}
		}
	}
	if bv[4] != "" && av[4] == "" {
		return false
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumber(av[i], bv[i]); c != 0 {
			return c > 0
		}
	}
	if av[4] == bv[4] {
		return false
	}
	if av[4] == "" {
		return true
	}
	if bv[4] == "" {
		return false
	}
	ap, bp := strings.Split(av[4], "."), strings.Split(bv[4], ".")
	for i := 0; i < min(len(ap), len(bp)); i++ {
		a, b := ap[i], bp[i]
		if a == b {
			continue
		}
		an, bn := numeric(a), numeric(b)
		if an && bn {
			return compareNumber(a, b) > 0
		}
		if an != bn {
			return !an
		}
		return a > b
	}
	return len(ap) > len(bp)
}

func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func compareNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

var upgradeNotices = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

// Surfaces an upgrade notice is reported on. Each keeps its own once-per-
// release guard, so a channel event the session never read does not hide the
// notice from the next check_inbox.
const (
	UpgradeSurfaceInbox   = "inbox"
	UpgradeSurfaceChannel = "channel"
	UpgradeSurfaceListen  = "listen"
)

// ReportUpgrade emits an actionable notice at most once per process per
// release on the inbox surface. A failed emission may be retried.
func ReportUpgrade(version string, emit func(string) error) error {
	return ReportUpgradeOn(UpgradeSurfaceInbox, version, emit)
}

// ReportUpgradeOn is ReportUpgrade for one surface; concurrent callers on the
// same surface share its guard.
func ReportUpgradeOn(surface, version string, emit func(string) error) error {
	return ReportUpgradeReload(surface, version, "", emit)
}

// ReportUpgradeReload is ReportUpgradeOn for a tincan mcp server: reload,
// when set, is how its app starts a fresh server (for example "quit Claude
// Code and start it again"), and the notice names it, since that server
// keeps the old build until then.
func ReportUpgradeReload(surface, version, reload string, emit func(string) error) error {
	if !Newer(version, Version) {
		return nil
	}
	version = strings.TrimPrefix(version, "v")
	upgradeNotices.Lock()
	defer upgradeNotices.Unlock()
	key := surface + "\x00" + version
	if upgradeNotices.seen[key] {
		return nil
	}
	restart := "restart long-running tincan processes"
	if reload != "" {
		restart = reload + " so this tincan mcp server runs the new build, and restart any other long-running tincan processes"
	}
	line := fmt.Sprintf("tincan %s is available from the relay (you run %s): run tincan upgrade, then %s.\n", version, strings.TrimPrefix(Version, "v"), restart)
	if err := emit(line); err != nil {
		return err
	}
	upgradeNotices.seen[key] = true
	return nil
}

// UpgradeNotice returns the next upgrade notice, or empty if already reported.
func UpgradeNotice(version string) string {
	var line string
	_ = ReportUpgrade(version, func(s string) error { line = s; return nil })
	return line
}

// UpgradeNoticeReload is UpgradeNotice for a tincan mcp server whose app
// reloads it with reload; it shares the inbox surface's guard.
func UpgradeNoticeReload(version, reload string) string {
	var line string
	_ = ReportUpgradeReload(UpgradeSurfaceInbox, version, reload, func(s string) error { line = s; return nil })
	return line
}
