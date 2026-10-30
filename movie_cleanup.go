package main

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
)

// Movies are deleted after watching (v2.18, owner 2026-10-29: "if I don't decide to archive it within 24 hours
// after watching, it can go"). A watched movie is queued like an episode; once the grace period has passed it is
// deleted through Radarr, files and all, with an import exclusion so no list or search brings it back. Only movies
// in the folders of MOVIE_DELETE_USERS (USER_FOLDERS keys) are touched, and only after that folder's owner watched
// them (WATCHERS, owners.go). Kept instead:
//   - movies tagged MOVIE_ARCHIVE_TAG or ♥'d by JELLYFIN_ARCHIVE_USERS (ArchiveMovies copies those to the archive),
//   - movies Radarr doesn't manage (the archive, manual downloads),
//   - a movie that is playing when it comes due (retried on the next run).

// errPlaying keeps a queue entry without counting a failed attempt: the file is being played right now.
var errPlaying = errors.New("playing right now")

// movieDeleteUser reports whether user (a USER_FOLDERS key) has movie deletion on.
func (a *App) movieDeleteUser(user string) bool {
	return slices.ContainsFunc(a.Config.MovieDeleteUsers, func(u string) bool { return userName(u) == userName(user) })
}

// findRadarrMovie returns the Radarr movie a queued movie refers to: by TMDB id, else by title and year when exactly
// one movie matches. nil if Radarr doesn't have it.
func findRadarrMovie(movies []radarrMovie, it PendingItem) *radarrMovie {
	if it.TmdbID != 0 {
		for i := range movies {
			if movies[i].TmdbID == it.TmdbID {
				return &movies[i]
			}
		}
		return nil
	}
	var found *radarrMovie
	for i := range movies {
		m := &movies[i]
		if normalizeTitle(m.Title) != normalizeTitle(it.Title) || (it.Year != 0 && m.Year != it.Year) {
			continue
		}
		if found != nil {
			return nil // ambiguous: keep both
		}
		found = m
	}
	return found
}

// applyMovie handles one due movie. done=true: it can leave the queue.
func (a *App) applyMovie(it PendingItem) (bool, error) {
	if a.Radarr == nil || len(a.Config.MovieDeleteUsers) == 0 {
		return true, nil
	}
	var movies []radarrMovie
	if err := a.Radarr.do("GET", "/api/v3/movie", nil, &movies); err != nil {
		return false, err
	}
	m := findRadarrMovie(movies, it)
	if m == nil {
		log.Printf("%s: not in Radarr (archive or manual download), keeping", it)
		return true, nil
	}
	if !m.HasFile || m.MovieFile == nil {
		a.debugf("%s: no file in Radarr", it)
		return true, nil
	}
	path := m.MovieFile.Path
	tags, err := a.Radarr.TagMap()
	if err != nil {
		return false, err
	}
	for _, id := range m.Tags {
		if tags[id] == a.Config.MovieArchiveTag {
			log.Printf("%s: tagged %s, keeping it for the archive", it, a.Config.MovieArchiveTag)
			return true, nil
		}
	}
	if a.Jellyfin != nil && len(a.Config.JellyfinArchiveUsers) > 0 {
		fav, err := a.Jellyfin.FavoriteMovies(a.Config.JellyfinArchiveUsers)
		if err != nil {
			return false, fmt.Errorf("jellyfin favourites: %w", err) // a ♥ must never be missed
		}
		if fav[m.TmdbID] {
			log.Printf("%s: favourite in Jellyfin, keeping it for the archive", it)
			return true, nil
		}
	}
	owner := a.folderUser(path, a.Config.MovieSubdir)
	if owner == "" || !a.movieDeleteUser(owner) {
		log.Printf("%s: keeping %s: not in a folder with movie deletion (MOVIE_DELETE_USERS)", it, path)
		return true, nil
	}
	if len(a.Config.Watchers) == 0 || len(it.Watchers) == 0 {
		log.Printf("%s: keeping %s: can't tell who watched it (WATCHERS)", it, path)
		return true, nil
	}
	if ok, wait, why := a.ownerCheck(it, path); !ok {
		if wait {
			return false, errOwnerWait
		}
		log.Printf("%s: keeping %s: %s", it, path, why)
		metrics.Inc("curator_actions_total", "action", "kept_not_owner")
		return true, nil
	}
	if a.playingFiles()[strings.ToLower(filepath.Base(path))] {
		return false, errPlaying
	}
	if err := a.deletable(path); err != nil {
		log.Printf("%s: refusing to delete: %v", it, err)
		return true, nil
	}
	if a.Config.DryRun {
		log.Printf("[dry run] %s: would delete %s and remove the movie from Radarr with an exclusion", it, path)
		return true, nil
	}
	if err := a.Radarr.do("DELETE", fmt.Sprintf("/api/v3/movie/%d?deleteFiles=true&addImportExclusion=true", m.ID), nil, nil); err != nil {
		return false, err
	}
	log.Printf("%s: watched, not archived: deleted %s and removed it from Radarr with an exclusion", it, path)
	metrics.Inc("curator_actions_total", "action", "movie_deleted")
	metrics.Add("curator_freed_bytes_total", float64(m.MovieFile.Size))
	return true, nil
}
