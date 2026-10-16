package main

import (
	"reflect"
	"testing"
)

func TestProjectRemotes(t *testing.T) {
	for _, remote := range []string{
		"git@gitlab.com:team/subgroup/app.git",
		"https://gitlab.com/team/subgroup/app.git",
		"ssh://git@gitlab.example.com:2222/team/app.git",
	} {
		if got := projectToURL(remote); got != remote {
			t.Errorf("remote changed: %s", got)
		}
		if got := repositoryName(remote); got != "app" {
			t.Errorf("name: %s", got)
		}
		if !isGitLab(remote) {
			t.Errorf("GitLab not detected: %s", remote)
		}
	}
	if got := projectToURL("owner/repo"); got != "git@github.com:owner/repo.git" {
		t.Fatal(got)
	}
	if isGitLab(projectToURL("owner/repo")) {
		t.Fatal("legacy GitHub classified as GitLab")
	}
}

func TestMergeRequestArguments(t *testing.T) {
	cmd := mergeRequestCommand("A title", "Text request\nwith details", "xmuggle-task")
	want := []string{"glab", "mr", "create", "--title", "A title", "--description", "Text request\nwith details", "--source-branch", "xmuggle-task", "--fill", "--push=false", "--yes"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("%q", cmd.Args)
	}
}
