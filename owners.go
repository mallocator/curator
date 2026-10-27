package main

import (
	"errors"
	"fmt"
	"strings"
)

// Deleting after watching needs the folder's owner (WATCHERS, v2.17). A file in a user's folder (USER_FOLDERS) is
// deleted only after that user watched it; another person's viewing keeps it (owner, 2026-10-26: a show Ravi
// downloaded and Daniela had already seen stays until he watched it). WATCHERS maps the accounts that report
// viewings to USER_FOLDERS keys: Jellyfin user names, Plex account titles (webhook) and "owner" (the Plex poll), e.g.
// "mallox=1-mallox,owner=1-mallox,daniela=2-daniela,gare347=2-daniela". The grace period counts from the owner's
// viewing. Unchanged (any viewing counts): files outside every user folder, archiving (it keeps a copy), queue
// entries recorded before v2.17 (no viewers stored), and everything while WATCHERS is empty.

// errOwnerWait keeps a queue entry without counting a failed attempt: the owner watched, but within the grace period.
var errOwnerWait = errors.New("owner's grace period not over")

// watcherUser returns the USER_FOLDERS key an account belongs to, or "".
func (a *App) watcherUser(account string) string {
	n := normalizeTitle(account)
	if n == "" {
		return ""
	}
	for acc, u := range a.Config.Watchers {
		if normalizeTitle(acc) == n {
			return u
		}
	}
	return ""
}

// ownerCheck decides whether the watched file at path may be deleted now. ok: delete. wait: the owner watched it,
// less than the grace period ago. Neither: keep it for good (why says who watched it).
func (a *App) ownerCheck(it PendingItem, path string) (ok, wait bool, why string) {
	if len(a.Config.Watchers) == 0 || len(it.Watchers) == 0 {
		return true, false, ""
	}
	owner := a.folderUser(path, "")
	if owner == "" {
		return true, false, ""
	}
	var who []string
	for _, w := range it.Watchers {
		if u := a.watcherUser(w.User); u != "" && userName(u) == userName(owner) {
			if a.now().Sub(w.At) >= a.Config.GracePeriod {
				return true, false, ""
			}
			return false, true, ""
		}
		name := w.User
		if name == "" {
			name = "an unknown " + w.Source + " user"
		}
		who = append(who, name)
	}
	return false, false, fmt.Sprintf("it's in %s's folder and was watched only by %s", userName(owner), strings.Join(who, ", "))
}
