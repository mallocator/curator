package main

import (
	"slices"
	"testing"
)

func TestSearchDirsDefaultToUserFolders(t *testing.T) {
	t.Setenv("USER_FOLDERS", "2-alice=/downloads/alice,1-mallox=/downloads/bob/,bob=/downloads/bob")
	t.Setenv("SEARCH_DIRS", "")
	if got, want := loadConfig().SearchDirs, []string{"/downloads/alice", "/downloads/bob"}; !slices.Equal(got, want) {
		t.Errorf("SearchDirs = %v, want %v", got, want)
	}
}

func TestSearchDirsSetWinsOverUserFolders(t *testing.T) {
	t.Setenv("USER_FOLDERS", "2-alice=/downloads/alice")
	t.Setenv("SEARCH_DIRS", "/media/a, /media/b")
	if got, want := loadConfig().SearchDirs, []string{"/media/a", "/media/b"}; !slices.Equal(got, want) {
		t.Errorf("SearchDirs = %v, want %v", got, want)
	}
}
