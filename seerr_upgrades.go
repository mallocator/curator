package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Better versions through Seerr (SEERR_UPGRADE_INTERVAL, default 15m; 0 disables).
//
// A movie that is already in the library shows as "available" in Seerr and in apps such as Moonfin, so it can't be
// requested again. Instead, a **Video** issue reported on it ("Report an issue" → Video, any text) asks for a better
// version:
//   - curator puts the movie (back) into Radarr if it isn't there (archived movies were removed with an import
//     exclusion: the exclusion is lifted), unmonitored, quality profile SEERR_UPGRADE_PROFILE, tagged for the archive
//     and with the reporter's user tag;
//   - an interactive search; the first release Radarr approves that beats the best version already there is grabbed:
//     a higher resolution, or the same resolution at SeerrUpgradeMinSizeRatio x the size. Existing versions come from
//     Jellyfin (archive copies) and from Radarr's own file;
//   - the archive step (movies.go) copies the download next to the old version (another resolution label) and Jellyfin
//     merges the versions: nothing in the archive is replaced or deleted (only the owner does that);
//   - every step is a comment on the issue; it is resolved once the new version is in the archive. Without a better
//     release the search repeats daily for SEERR_UPGRADE_DAYS, then curator gives up, says so, and restores Radarr
//     (a movie it added is removed again with its exclusion);
//   - an issue closed by hand stops the work the same way.
// Only movies; Video issues on shows are left alone. State: SEERR_UPGRADE_STATE_FILE.

const (
	seerrIssueVideo  = 1
	seerrIssueOpen   = 1
	seerrUpgradeKeep = 30 * 24 * time.Hour // finished entries are forgotten after this
)

// SeerrUpgradeMinSizeRatio: a release at the same resolution must be this much larger to count as better.
var SeerrUpgradeMinSizeRatio = 1.5

type seerrIssue struct {
	ID        int `json:"id"`
	IssueType int `json:"issueType"`
	Status    int `json:"status"`
	Media     struct {
		TmdbID    int    `json:"tmdbId"`
		MediaType string `json:"mediaType"`
	} `json:"media"`
	CreatedBy struct {
		ID          int    `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"createdBy"`
}

// OpenIssues returns Seerr's open issues.
func (s *Seerr) OpenIssues() ([]seerrIssue, error) {
	var out []seerrIssue
	for skip := 0; ; skip += 50 {
		var page struct {
			PageInfo struct {
				Results int `json:"results"`
			} `json:"pageInfo"`
			Results []seerrIssue `json:"results"`
		}
		if _, err := s.do("GET", fmt.Sprintf("/api/v1/issue?take=50&skip=%d&sort=added&filter=open", skip), nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Results...)
		if len(page.Results) == 0 || skip+50 >= page.PageInfo.Results {
			return out, nil
		}
	}
}

func (s *Seerr) CommentIssue(id int, msg string) error {
	_, err := s.do("POST", fmt.Sprintf("/api/v1/issue/%d/comment", id), map[string]string{"message": msg}, nil)
	return err
}

func (s *Seerr) ResolveIssue(id int) error {
	_, err := s.do("POST", fmt.Sprintf("/api/v1/issue/%d/resolved", id), nil, nil)
	return err
}

// mediaVersion is one copy of a film: its vertical resolution class (480/720/1080/2160) and size.
type mediaVersion struct {
	Resolution int
	Size       int64
}

func (v mediaVersion) String() string {
	return fmt.Sprintf("%dp, %.1f GB", v.Resolution, float64(v.Size)/(1<<30))
}

// resolutionClass maps a frame height to the usual class (a 1920x800 film is "1080p").
func resolutionClass(height, width int) int {
	switch {
	case height >= 1500 || width >= 3000:
		return 2160
	case height >= 900 || width >= 1700:
		return 1080
	case height >= 600 || width >= 1100:
		return 720
	case height > 0:
		return 480
	}
	return 0
}

// MovieVersions returns the versions of a film (TMDB id) below pathPrefix, read from Jellyfin's media sources.
func (j *Jellyfin) MovieVersions(tmdbID int, pathPrefix string) ([]mediaVersion, error) {
	var items struct {
		Items []struct {
			Path         string            `json:"Path"`
			ProviderIds  map[string]string `json:"ProviderIds"`
			MediaSources []struct {
				Size         int64 `json:"Size"`
				MediaStreams []struct {
					Type   string `json:"Type"`
					Height int    `json:"Height"`
					Width  int    `json:"Width"`
				} `json:"MediaStreams"`
			} `json:"MediaSources"`
		} `json:"Items"`
	}
	// all movies, filtered here (Jellyfin ignores AnyProviderIdEquals)
	if err := j.allMovies("ProviderIds,Path,MediaSources", &items); err != nil {
		return nil, err
	}
	var out []mediaVersion
	for _, it := range items.Items {
		if pathPrefix != "" && !strings.HasPrefix(it.Path, strings.TrimRight(pathPrefix, "/")+"/") {
			continue
		}
		match := false
		for k, v := range it.ProviderIds {
			match = match || (strings.EqualFold(k, "tmdb") && v == strconv.Itoa(tmdbID))
		}
		if !match {
			continue
		}
		for _, src := range it.MediaSources {
			v := mediaVersion{Size: src.Size}
			for _, st := range src.MediaStreams {
				if st.Type == "Video" {
					v.Resolution = max(v.Resolution, resolutionClass(st.Height, st.Width))
				}
			}
			out = append(out, v)
		}
	}
	return out, nil
}

// bestVersion is the highest-resolution version, the larger one among equals.
func bestVersion(vs []mediaVersion) (mediaVersion, bool) {
	if len(vs) == 0 {
		return mediaVersion{}, false
	}
	best := vs[0]
	for _, v := range vs[1:] {
		if v.Resolution > best.Resolution || (v.Resolution == best.Resolution && v.Size > best.Size) {
			best = v
		}
	}
	return best, true
}

// betterThan reports whether release r beats the best existing version.
func betterThan(r arrRelease, have []mediaVersion) bool {
	best, ok := bestVersion(have)
	if !ok {
		return true
	}
	res := r.Quality.Quality.Resolution
	return res > best.Resolution || (res == best.Resolution && float64(r.Size) >= SeerrUpgradeMinSizeRatio*float64(best.Size))
}

// pickUpgrade returns the first release Radarr approves that beats the existing versions (Radarr's own order).
func pickUpgrade(rs []arrRelease, have []mediaVersion) (arrRelease, bool) {
	for _, r := range rs {
		if r.Approved && betterThan(r, have) {
			return r, true
		}
	}
	return arrRelease{}, false
}

type seerrUpgradeRecord struct {
	Tmdb       int       `json:"tmdb"`
	Title      string    `json:"title,omitempty"`
	Stage      string    `json:"stage"` // searching, grabbed, done, gave_up, closed
	RadarrID   int       `json:"radarr_id,omitempty"`
	Added      bool      `json:"added,omitempty"` // curator put the movie into Radarr (remove it again when giving up)
	Started    time.Time `json:"started"`
	LastSearch time.Time `json:"last_search,omitzero"`
	GrabbedAt  time.Time `json:"grabbed_at,omitzero"`
	Release    string    `json:"release,omitempty"`
	Told       bool      `json:"told,omitempty"` // "nothing better yet" was commented
	Finished   time.Time `json:"finished,omitzero"`
}

type seerrUpgradeLog struct {
	mu    sync.Mutex
	path  string
	items map[int]*seerrUpgradeRecord
}

func (l *seerrUpgradeLog) save() {
	if l.path == "" {
		return
	}
	data, err := json.MarshalIndent(l.items, "", " ")
	if err == nil {
		tmp := l.path + ".tmp"
		if err = os.WriteFile(tmp, data, 0o644); err == nil {
			err = os.Rename(tmp, l.path)
		}
	}
	if err != nil {
		log.Printf("Seerr upgrades: saving %s: %v", l.path, err)
	}
}

func (a *App) seerrUpgradeLog() *seerrUpgradeLog {
	a.seerrUpgradesOnce.Do(func() {
		l := &seerrUpgradeLog{path: a.Config.SeerrUpgradeStateFile, items: map[int]*seerrUpgradeRecord{}}
		if data, err := os.ReadFile(l.path); err == nil {
			if err := json.Unmarshal(data, &l.items); err != nil {
				log.Printf("Seerr upgrades: reading %s: %v", l.path, err)
			}
		} else if l.path != "" && !errors.Is(err, os.ErrNotExist) {
			log.Printf("Seerr upgrades: reading %s: %v", l.path, err)
		}
		a.seerrUpgrades = l
	})
	return a.seerrUpgrades
}

// SeerrUpgrades handles Video issues on movies (see the top of this file). Runs every SEERR_UPGRADE_INTERVAL.
func (a *App) SeerrUpgrades() {
	if a.Seerr == nil || a.Radarr == nil {
		return
	}
	issues, err := a.Seerr.OpenIssues()
	if err != nil {
		log.Printf("Seerr upgrades: %v", err)
		return
	}
	l := a.seerrUpgradeLog()
	l.mu.Lock()
	defer l.mu.Unlock()
	defer l.save()
	open := map[int]bool{}
	for _, is := range issues {
		if is.IssueType != seerrIssueVideo || is.Status != seerrIssueOpen || is.Media.MediaType != "movie" || is.Media.TmdbID == 0 {
			continue
		}
		open[is.ID] = true
		rec := l.items[is.ID]
		if rec == nil {
			rec = &seerrUpgradeRecord{Tmdb: is.Media.TmdbID, Stage: "searching", Started: a.now()}
			l.items[is.ID] = rec
		}
		switch rec.Stage {
		case "searching":
			a.seerrUpgradeSearch(is, rec)
		case "grabbed":
			a.seerrUpgradeCheck(is, rec)
		}
	}
	// issues closed by hand (or by us) and old entries
	for id, rec := range l.items {
		if !open[id] && (rec.Stage == "searching" || rec.Stage == "grabbed") {
			log.Printf("Seerr upgrades: issue %d (%s) was closed; stopping", id, rec.Title)
			if rec.Stage == "searching" {
				a.seerrUpgradeRestore(rec)
			}
			rec.Stage, rec.Finished = "closed", a.now()
		}
		if !rec.Finished.IsZero() && a.now().Sub(rec.Finished) > seerrUpgradeKeep {
			delete(l.items, id)
		}
	}
}

func (a *App) seerrUpgradeSearch(is seerrIssue, rec *seerrUpgradeRecord) {
	if !rec.LastSearch.IsZero() && a.now().Sub(rec.LastSearch) < 24*time.Hour {
		return
	}
	if a.Config.SeerrUpgradeDays > 0 && a.now().Sub(rec.Started) > time.Duration(a.Config.SeerrUpgradeDays)*24*time.Hour {
		a.seerrComment(is.ID, fmt.Sprintf("No better version of %s turned up in %d days, so I stopped looking. Report a new Video issue to try again.", rec.Title, a.Config.SeerrUpgradeDays))
		a.seerrUpgradeRestore(rec)
		rec.Stage, rec.Finished = "gave_up", a.now()
		metrics.Inc("curator_actions_total", "action", "seerr_upgrade_gave_up")
		return
	}
	m, added, err := a.radarrMovieFor(is)
	if err != nil {
		log.Printf("Seerr upgrades: issue %d (tmdb %d): %v", is.ID, is.Media.TmdbID, err)
		return
	}
	rec.Title, rec.RadarrID, rec.Added = fmt.Sprintf("%s (%d)", m.Title, m.Year), m.ID, rec.Added || added
	var have []mediaVersion
	if a.Jellyfin != nil {
		if vs, err := a.Jellyfin.MovieVersions(is.Media.TmdbID, a.Config.MovieArchiveHostPath); err == nil {
			have = vs
		} else {
			log.Printf("Seerr upgrades: %s: jellyfin versions: %v", rec.Title, err)
		}
	}
	if m.HasFile && m.MovieFile != nil {
		have = append(have, mediaVersion{Resolution: m.MovieFile.Quality.Quality.Resolution, Size: m.MovieFile.Size})
	}
	rec.LastSearch = a.now()
	if a.Config.DryRun {
		log.Printf("[dry run] Seerr upgrades: would search a better version of %s (have %d version(s))", rec.Title, len(have))
		return
	}
	rs, err := a.Radarr.Releases(fmt.Sprintf("movieId=%d", m.ID))
	if err != nil {
		log.Printf("Seerr upgrades: %s: search: %v", rec.Title, err)
		return
	}
	best, haveAny := bestVersion(have)
	r, ok := pickUpgrade(rs, have)
	if !ok {
		log.Printf("Seerr upgrades: %s: nothing better than %s among %d releases", rec.Title, describeHave(best, haveAny), len(rs))
		if !rec.Told {
			a.seerrComment(is.ID, fmt.Sprintf("Looking for a better version of %s than the current one (%s): nothing better is available right now. I'll look again every day for %d days.", rec.Title, describeHave(best, haveAny), a.Config.SeerrUpgradeDays))
			rec.Told = true
		}
		return
	}
	if err := a.Radarr.Grab(r); err != nil {
		log.Printf("Seerr upgrades: %s: grabbing %s: %v", rec.Title, r.Title, err)
		return
	}
	rec.Stage, rec.GrabbedAt, rec.Release = "grabbed", a.now(), r.Title
	log.Printf("Seerr upgrades: grabbed %s for %s (better than %s)", r.Title, rec.Title, describeHave(best, haveAny))
	a.seerrComment(is.ID, fmt.Sprintf("Downloading a better version of %s: %s (%dp, %.1f GB; current: %s). It goes into the archive next to the current version; the issue closes when it's there.",
		rec.Title, r.Title, r.Quality.Quality.Resolution, float64(r.Size)/(1<<30), describeHave(best, haveAny)))
	metrics.Inc("curator_actions_total", "action", "seerr_upgrade_grabbed")
}

func describeHave(v mediaVersion, ok bool) string {
	if !ok {
		return "unknown"
	}
	return v.String()
}

// seerrUpgradeCheck: done once the movie has left Radarr (the archive step removes it after copying).
func (a *App) seerrUpgradeCheck(is seerrIssue, rec *seerrUpgradeRecord) {
	var m radarrMovie
	err := a.Radarr.do("GET", fmt.Sprintf("/api/v3/movie/%d", rec.RadarrID), nil, &m)
	if err != nil && strings.Contains(err.Error(), "404") {
		log.Printf("Seerr upgrades: %s is in the archive; resolving issue %d", rec.Title, is.ID)
		a.seerrComment(is.ID, fmt.Sprintf("Done: the new version of %s (%s) is in the archive next to the old one; Jellyfin shows both as versions of the film. Delete the old file whenever you like.", rec.Title, rec.Release))
		if err := a.Seerr.ResolveIssue(is.ID); err != nil {
			log.Printf("Seerr upgrades: resolving issue %d: %v", is.ID, err)
		}
		rec.Stage, rec.Finished = "done", a.now()
		metrics.Inc("curator_actions_total", "action", "seerr_upgrade_done")
		return
	}
	if err != nil {
		log.Printf("Seerr upgrades: %s: %v", rec.Title, err)
		return
	}
	if a.now().Sub(rec.GrabbedAt) > 3*24*time.Hour {
		log.Printf("Seerr upgrades: %s: download of %s not archived after 3 days; searching again", rec.Title, rec.Release)
		a.seerrComment(is.ID, fmt.Sprintf("The download of %s didn't finish within 3 days; looking for another release.", rec.Release))
		rec.Stage, rec.LastSearch, rec.Release = "searching", time.Time{}, ""
	}
}

// radarrMovieFor returns the Radarr movie for the issue's film, adding it (unmonitored, upgrade profile, archive and
// reporter tags) when it isn't there. added reports that curator added it.
func (a *App) radarrMovieFor(is seerrIssue) (radarrMovie, bool, error) {
	var found []radarrMovie
	if err := a.Radarr.do("GET", fmt.Sprintf("/api/v3/movie?tmdbId=%d", is.Media.TmdbID), nil, &found); err != nil {
		return radarrMovie{}, false, err
	}
	if len(found) > 0 {
		return found[0], false, nil
	}
	var excl []struct {
		ID     int `json:"id"`
		TmdbID int `json:"tmdbId"`
	}
	if err := a.Radarr.do("GET", "/api/v3/exclusions", nil, &excl); err != nil {
		return radarrMovie{}, false, err
	}
	for _, e := range excl {
		if e.TmdbID == is.Media.TmdbID {
			if err := a.Radarr.do("DELETE", fmt.Sprintf("/api/v3/exclusions/%d", e.ID), nil, nil); err != nil {
				return radarrMovie{}, false, err
			}
		}
	}
	var lookup map[string]any
	if err := a.Radarr.do("GET", fmt.Sprintf("/api/v3/movie/lookup/tmdb?tmdbId=%d", is.Media.TmdbID), nil, &lookup); err != nil {
		return radarrMovie{}, false, err
	}
	profile, root, tags, err := a.seerrUpgradeTargets(is)
	if err != nil {
		return radarrMovie{}, false, err
	}
	lookup["qualityProfileId"], lookup["rootFolderPath"], lookup["tags"] = profile, root, tags
	lookup["monitored"], lookup["minimumAvailability"] = false, "released"
	lookup["addOptions"] = map[string]any{"searchForMovie": false, "monitor": "none"}
	var m radarrMovie
	if a.Config.DryRun {
		return radarrMovie{TmdbID: is.Media.TmdbID, Title: fmt.Sprint(lookup["title"])}, false, nil
	}
	if err := a.Radarr.do("POST", "/api/v3/movie", lookup, &m); err != nil {
		return radarrMovie{}, false, err
	}
	log.Printf("Seerr upgrades: added %s (%d) to Radarr for issue %d (unmonitored, tagged %v)", m.Title, m.Year, is.ID, tags)
	return m, true, nil
}

// seerrUpgradeTargets: quality profile id, root folder and tags (archive tag, created if missing, and the reporter's
// USER_FOLDERS tag) for a movie added for an upgrade.
func (a *App) seerrUpgradeTargets(is seerrIssue) (int, string, []int, error) {
	var profiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := a.Radarr.do("GET", "/api/v3/qualityprofile", nil, &profiles); err != nil {
		return 0, "", nil, err
	}
	profile := 0
	for _, p := range profiles {
		if strings.EqualFold(p.Name, a.Config.SeerrUpgradeProfile) {
			profile = p.ID
		}
	}
	if profile == 0 {
		return 0, "", nil, fmt.Errorf("quality profile %q not in Radarr", a.Config.SeerrUpgradeProfile)
	}
	var roots []struct {
		Path string `json:"path"`
	}
	if err := a.Radarr.do("GET", "/api/v3/rootfolder", nil, &roots); err != nil {
		return 0, "", nil, err
	}
	if len(roots) == 0 {
		return 0, "", nil, fmt.Errorf("radarr has no root folder")
	}
	root := roots[0].Path
	tagMap, err := a.Radarr.TagMap()
	if err != nil {
		return 0, "", nil, err
	}
	var tags []int
	want := []string{a.Config.MovieArchiveTag}
	for key := range a.Config.UserFolders {
		if n, err := strconv.Atoi(strings.SplitN(key, "-", 2)[0]); err == nil && n == is.CreatedBy.ID {
			want = append(want, key)
		}
	}
	for _, label := range want {
		if label == "" {
			continue
		}
		id := -1
		for tid, l := range tagMap {
			if strings.EqualFold(l, label) {
				id = tid
			}
		}
		if id < 0 {
			var t SonarrTag
			if err := a.Radarr.do("POST", "/api/v3/tag", map[string]string{"label": label}, &t); err != nil {
				return 0, "", nil, err
			}
			id = t.ID
		}
		if !slices.Contains(tags, id) {
			tags = append(tags, id)
		}
	}
	return profile, root, tags, nil
}

// seerrUpgradeRestore removes a movie curator added (and nothing was grabbed for) and restores its import exclusion.
func (a *App) seerrUpgradeRestore(rec *seerrUpgradeRecord) {
	if !rec.Added || rec.RadarrID == 0 || a.Config.DryRun {
		return
	}
	if err := a.Radarr.do("DELETE", fmt.Sprintf("/api/v3/movie/%d?deleteFiles=false&addImportExclusion=true", rec.RadarrID), nil, nil); err != nil {
		log.Printf("Seerr upgrades: removing %s from Radarr again: %v", rec.Title, err)
		return
	}
	log.Printf("Seerr upgrades: removed %s from Radarr again (exclusion restored)", rec.Title)
}

func (a *App) seerrComment(id int, msg string) {
	if a.Config.DryRun {
		log.Printf("[dry run] Seerr upgrades: would comment on issue %d: %s", id, msg)
		return
	}
	if err := a.Seerr.CommentIssue(id, msg); err != nil {
		log.Printf("Seerr upgrades: commenting on issue %d: %v", id, err)
	}
}
