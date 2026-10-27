package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appWithOwners is appWithSonarr with the fake Sonarr's media folder as Ravi's download folder.
func appWithOwners(t *testing.T, watchers map[string]string) (*App, *fakeSonarr) {
	a, fs := appWithSonarr(t)
	// files are <media>/tv/<show>/<season>/<file>: <media> is Ravi's base folder
	base := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(fs.files[11].Path))))
	a.Config.UserFolders = map[string]string{"1-mallox": base, "2-daniela": "/downloads/daniela"}
	a.Config.Watchers = watchers
	return a, fs
}

var houseWatchers = map[string]string{"mallox": "1-mallox", "owner": "1-mallox", "Daniela": "2-daniela", "gare347": "2-daniela"}

func watchBy(a *App, user, series string, season, episode int, ago time.Duration) {
	a.Queue.Add(WatchEvent{Source: "jellyfin", Type: "episode", Series: series, Season: season, Episode: episode, User: user},
		a.now().Add(-ago))
}

func TestOtherUsersViewingKeepsOwnersFile(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	path := fs.files[11].Path
	watchBy(a, "Daniela", "The Simpsons", 38, 2, 30*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v: Daniela's viewing must not delete Ravi's file", fs.deleted)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("file should still be there")
	}
	if len(a.Queue.Items()) != 0 {
		t.Fatalf("a kept item leaves the queue: %+v", a.Queue.Items())
	}
	// Ravi watches it later: now it goes, after his grace period.
	watchBy(a, "mallox", "The Simpsons", 38, 2, 25*time.Hour)
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestGracePeriodCountsFromOwnersViewing(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	watchBy(a, "Daniela", "The Simpsons", 38, 2, 30*time.Hour)
	watchBy(a, "mallox", "The Simpsons", 38, 2, 2*time.Hour) // joins the queued item
	items := a.Queue.Items()
	if len(items) != 1 || len(items[0].Watchers) != 2 {
		t.Fatalf("queue %+v", items)
	}
	a.ProcessDue()
	if len(fs.deleted) != 0 || len(a.Queue.Items()) != 1 || a.Queue.Items()[0].Attempts != 0 {
		t.Fatalf("within Ravi's grace period: deleted %v, queue %+v", fs.deleted, a.Queue.Items())
	}
	later := a.now().Add(23 * time.Hour)
	a.Now = func() time.Time { return later }
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" || len(a.Queue.Items()) != 0 {
		t.Fatalf("after Ravi's grace period: deleted %v, queue %+v", fs.deleted, a.Queue.Items())
	}
}

func TestPlexPollOwnerCounts(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	a.Queue.Add(WatchEvent{Source: "plex-poll", Type: "episode", Series: "The Simpsons", Season: 38, Episode: 2, User: "owner"},
		a.now().Add(-25*time.Hour))
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestUnknownViewerKeepsOwnersFile(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	watchBy(a, "", "The Simpsons", 38, 2, 30*time.Hour)
	a.ProcessDue()
	if len(fs.deleted) != 0 {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestOwnerRuleOffOrLegacyItems(t *testing.T) {
	// WATCHERS empty: any viewing counts, as before.
	a, fs := appWithOwners(t, nil)
	watchBy(a, "Daniela", "The Simpsons", 38, 2, 30*time.Hour)
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" {
		t.Fatalf("rule off: deleted %v", fs.deleted)
	}
	// An entry queued before v2.17 has no watchers: any viewing counts.
	b, fs2 := appWithOwners(t, houseWatchers)
	watch(b, "The Simpsons", 38, 2, 30*time.Hour)
	b.Queue.mu.Lock()
	for k, it := range b.Queue.items {
		it.Watchers = nil
		b.Queue.items[k] = it
	}
	b.Queue.mu.Unlock()
	b.ProcessDue()
	if fmt.Sprint(fs2.deleted) != "[11]" {
		t.Fatalf("legacy entry: deleted %v", fs2.deleted)
	}
}

func TestOwnerRuleLeavesArchivingAlone(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	watchBy(a, "Daniela", "Futurama", 11, 10, 30*time.Hour) // archive tag: a copy is kept, so anyone's viewing counts
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[21]" {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestFilesOutsideUserFoldersFollowTheOldRule(t *testing.T) {
	a, fs := appWithOwners(t, houseWatchers)
	a.Config.UserFolders = map[string]string{"1-mallox": "/downloads/ravi", "2-daniela": "/downloads/daniela"}
	watchBy(a, "Daniela", "The Simpsons", 38, 2, 30*time.Hour)
	a.ProcessDue()
	if fmt.Sprint(fs.deleted) != "[11]" {
		t.Fatalf("deleted %v", fs.deleted)
	}
}

func TestByNameFilesNeedTheOwner(t *testing.T) {
	a := newTestApp(t)
	base := t.TempDir()
	f := filepath.Join(base, "MobLand.S02E03.1080p.WEB.h264-GRP.mkv")
	os.WriteFile(f, []byte("x"), 0644)
	a.Config.SearchDirs = []string{base}
	a.Config.DeleteShows = []string{"MobLand"}
	a.Config.UserFolders = map[string]string{"1-mallox": base}
	a.Config.Watchers = houseWatchers
	watchBy(a, "gare347", "MobLand", 2, 3, 30*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(f); err != nil {
		t.Fatal("Daniela's viewing must not delete Ravi's file")
	}
	watchBy(a, "mallox", "MobLand", 2, 3, 25*time.Hour)
	a.ProcessDue()
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("Ravi watched it: the file should be gone")
	}
}

func TestWebhooksReportTheViewer(t *testing.T) {
	a := newTestApp(t)
	srv := httptest.NewServer(a.Routes())
	defer srv.Close()
	rec := httptest.NewRecorder()
	a.handlePlex(rec, plexRequest(t, `{"event":"media.scrobble","Account":{"id":2,"title":"gare347"},
		"Metadata":{"type":"episode","grandparentTitle":"The Simpsons","parentIndex":38,"index":2,"title":"x"}}`))
	jf := httptest.NewRequest("POST", "/jellyfin", strings.NewReader(`{"NotificationType":"PlaybackStop","PlayedToCompletion":true,
		"NotificationUsername":"Daniela","ItemType":"Episode","SeriesName":"The Simpsons","SeasonNumber":38,"EpisodeNumber":2,"Name":"x"}`))
	jf.Header.Set("Content-Type", "application/json")
	a.handleJellyfin(httptest.NewRecorder(), jf)
	items := a.Queue.Items()
	if len(items) != 1 || len(items[0].Watchers) != 2 || items[0].Watchers[0].User != "gare347" || items[0].Watchers[1].User != "Daniela" {
		t.Fatalf("queue %+v", items)
	}
}
