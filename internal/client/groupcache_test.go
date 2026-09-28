package client

import (
	"fmt"
	"testing"
	"time"
)

func TestGroupCacheIsBounded(t *testing.T) {
	r := &Relay{}
	for i := range maxCachedGroups + 6 {
		r.cacheGroupLocked(GroupResult{Group: fmt.Sprintf("group-%d", i)})
		time.Sleep(time.Microsecond) // distinct send times
	}
	if len(r.groups) != maxCachedGroups {
		t.Fatalf("cached %d groups, want %d", len(r.groups), maxCachedGroups)
	}
	if _, ok := r.cachedGroupLocked("group-0"); ok {
		t.Fatal("oldest group was not evicted")
	}
	if _, ok := r.cachedGroupLocked(fmt.Sprintf("group-%d", maxCachedGroups+5)); !ok {
		t.Fatal("newest group was evicted")
	}
	r.groups["group-old"] = cachedGroup{g: GroupResult{Group: "group-old"}, at: time.Now().Add(-groupCacheTTL - time.Minute)}
	if _, ok := r.cachedGroupLocked("group-old"); ok {
		t.Fatal("expired group still served")
	}
	if _, ok := r.groups["group-old"]; ok {
		t.Fatal("expired group still held")
	}
}
