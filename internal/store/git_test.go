package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func testRepo(t *testing.T) *Store {
	t.Helper()
	cfg := config.Repo{
		Path:        filepath.Join(t.TempDir(), "configs"),
		Branch:      "main",
		Layout:      "flat",
		AuthorName:  "jconfig",
		AuthorEmail: "jconfig@example.com",
	}
	st, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return st
}

func commitCount(t *testing.T, s *Store) int {
	t.Helper()
	head, err := s.repo.Head()
	if err != nil {
		return 0
	}
	iter, err := s.repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()

	n := 0
	_ = iter.ForEach(func(*object.Commit) error { n++; return nil })
	return n
}

func TestOpenInitialisesRepo(t *testing.T) {
	s := testRepo(t)

	if _, err := os.Stat(filepath.Join(s.Path(), ".git")); err != nil {
		t.Fatalf("repository was not initialised: %v", err)
	}
	// Opening an existing repository must not fail.
	if _, err := Open(context.Background(), s.cfg); err != nil {
		t.Fatalf("re-Open: %v", err)
	}
}

func TestCommitOnlyWhenChanged(t *testing.T) {
	s := testRepo(t)

	req := CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "version 21.4;\n"},
		Message: "mx1: configuration changed",
		When:    time.Now(),
	}

	res, err := s.Commit(req)
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	if !res.Changed {
		t.Fatal("first commit should report a change")
	}
	if res.Hash == "" {
		t.Error("first commit should return a hash")
	}
	if got := commitCount(t, s); got != 1 {
		t.Errorf("commit count = %d, want 1", got)
	}

	// Same content again: no commit at all.
	res, err = s.Commit(req)
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if res.Changed {
		t.Error("an identical configuration must not produce a commit")
	}
	if got := commitCount(t, s); got != 1 {
		t.Errorf("commit count = %d, want 1 after an unchanged run", got)
	}

	// Changed content: a new commit.
	req.Files["mx1.conf"] = "version 21.4R3;\n"
	res, err = s.Commit(req)
	if err != nil {
		t.Fatalf("third Commit: %v", err)
	}
	if !res.Changed {
		t.Error("a changed configuration must produce a commit")
	}
	if got := commitCount(t, s); got != 2 {
		t.Errorf("commit count = %d, want 2", got)
	}

	content, err := os.ReadFile(filepath.Join(s.Path(), "mx1.conf"))
	if err != nil {
		t.Fatalf("read stored config: %v", err)
	}
	if string(content) != "version 21.4R3;\n" {
		t.Errorf("stored config = %q", content)
	}
}

func TestCommitNestedPath(t *testing.T) {
	s := testRepo(t)

	res, err := s.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"core/mx1.conf": "a\n", "core/mx1.set": "b\n"},
		Message: "mx1: configuration changed",
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !res.Changed || len(res.Files) != 2 {
		t.Fatalf("Commit result = %+v, want 2 changed files", res)
	}
	if _, err := os.Stat(filepath.Join(s.Path(), "core", "mx1.set")); err != nil {
		t.Errorf("grouped path was not created: %v", err)
	}
}

func TestHeadCommitAndDirty(t *testing.T) {
	s := testRepo(t)

	when, hash, err := s.HeadCommit()
	if err != nil {
		t.Fatalf("HeadCommit on an empty repo: %v", err)
	}
	if !when.IsZero() || hash != "" {
		t.Errorf("empty repo should report a zero head, got %v %q", when, hash)
	}

	if _, err := s.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "a\n"},
		Message: "mx1",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	when, hash, err = s.HeadCommit()
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if when.IsZero() || hash == "" {
		t.Errorf("HeadCommit = %v %q, want a real commit", when, hash)
	}

	dirty, err := s.Dirty()
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if dirty {
		t.Error("repository should be clean right after a commit")
	}

	// A leftover file from an interrupted run shows up as dirty.
	if err := os.WriteFile(filepath.Join(s.Path(), "stray.conf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = s.Dirty()
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if !dirty {
		t.Error("an untracked file should make the repository dirty")
	}
}

func TestPushAndUnpushed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available for the local transport")
	}

	// A bare repository standing in for the remote.
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	cfg := config.Repo{
		Path:        filepath.Join(t.TempDir(), "configs"),
		Branch:      "main",
		Layout:      "flat",
		AuthorName:  "jconfig",
		AuthorEmail: "jconfig@example.com",
		Push: config.Push{
			Enabled: true,
			Remote:  "origin",
			URL:     remote,
			Branch:  "main",
			Timeout: config.Duration(30 * time.Second),
		},
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := s.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "a\n"},
		Message: "mx1: first",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	ahead, err := s.Unpushed(context.Background())
	if err != nil {
		t.Fatalf("Unpushed before push: %v", err)
	}
	if ahead != 1 {
		t.Errorf("Unpushed = %d, want 1 before pushing", ahead)
	}

	pushed, err := s.Push(context.Background())
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !pushed {
		t.Error("Push should report that it moved the remote")
	}

	ahead, err = s.Unpushed(context.Background())
	if err != nil {
		t.Fatalf("Unpushed after push: %v", err)
	}
	if ahead != 0 {
		t.Errorf("Unpushed = %d, want 0 after pushing", ahead)
	}

	// Nothing new: the push is a no-op, not an error.
	pushed, err = s.Push(context.Background())
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if pushed {
		t.Error("an up-to-date remote should report no push")
	}
}

func TestPushFailureIsReported(t *testing.T) {
	no := false
	cfg := config.Repo{
		Path:        filepath.Join(t.TempDir(), "configs"),
		Branch:      "main",
		Layout:      "flat",
		AuthorName:  "jconfig",
		AuthorEmail: "jconfig@example.com",
		// Start locally: this test is about push, not about Open.
		CloneInit: &no,
		Push: config.Push{
			Enabled: true,
			Remote:  "origin",
			URL:     filepath.Join(t.TempDir(), "does-not-exist.git"),
			Branch:  "main",
			Timeout: config.Duration(10 * time.Second),
		},
	}
	s, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "a\n"},
		Message: "mx1",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if _, err := s.Push(context.Background()); err == nil {
		t.Fatal("pushing to a nonexistent remote must fail")
	}
}

// repoWithRemote returns a config pointing at path with remote as its push URL.
func repoWithRemote(path, remote string) config.Repo {
	return config.Repo{
		Path:        path,
		Branch:      "main",
		Layout:      "flat",
		AuthorName:  "jconfig",
		AuthorEmail: "jconfig@example.com",
		Push: config.Push{
			Enabled: true,
			Remote:  "origin",
			URL:     remote,
			Branch:  "main",
			Timeout: config.Duration(30 * time.Second),
		},
	}
}

// seedRemote creates a bare repository holding one commit.
func seedRemote(t *testing.T) string {
	t.Helper()
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	seed, err := Open(context.Background(), repoWithRemote(filepath.Join(t.TempDir(), "seed"), remote))
	if err != nil {
		t.Fatalf("open seed repo: %v", err)
	}
	if _, err := seed.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "version 21.4;\n"},
		Message: "mx1: seeded",
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	if _, err := seed.Push(context.Background()); err != nil {
		t.Fatalf("seed push: %v", err)
	}
	return remote
}

// A container starting on a blank volume must continue the remote's history,
// not begin an unrelated one whose first push would be rejected.
func TestOpenClonesExistingRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available for the local transport")
	}
	remote := seedRemote(t)

	fresh := filepath.Join(t.TempDir(), "configs")
	s, err := Open(context.Background(), repoWithRemote(fresh, remote))
	if err != nil {
		t.Fatalf("Open on a blank volume: %v", err)
	}

	// History and working tree came from the remote.
	body, err := os.ReadFile(filepath.Join(fresh, "mx1.conf"))
	if err != nil {
		t.Fatalf("cloned worktree is missing mx1.conf: %v", err)
	}
	if string(body) != "version 21.4;\n" {
		t.Errorf("cloned mx1.conf = %q", body)
	}
	if got := commitCount(t, s); got != 1 {
		t.Errorf("commit count = %d, want the remote's 1", got)
	}
	if ahead, err := s.Unpushed(context.Background()); err != nil || ahead != 0 {
		t.Errorf("Unpushed = %d, %v; a fresh clone must not be ahead", ahead, err)
	}

	// And a push from the clone is a fast-forward, not a rejected write.
	if _, err := s.Commit(CommitRequest{
		Device:  "mx2",
		Files:   map[string]string{"mx2.conf": "version 21.4;\n"},
		Message: "mx2: added",
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, err := s.Push(context.Background()); err != nil {
		t.Fatalf("push from a cloned repo must fast-forward: %v", err)
	}
}

// The very first deployment points at an empty remote; that must not be fatal.
func TestOpenFallsBackWhenRemoteIsEmpty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available for the local transport")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	path := filepath.Join(t.TempDir(), "configs")
	s, err := Open(context.Background(), repoWithRemote(path, remote))
	if err != nil {
		t.Fatalf("Open against an empty remote: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		t.Fatalf("repository was not initialised: %v", err)
	}
	if _, err := s.Commit(CommitRequest{
		Device:  "mx1",
		Files:   map[string]string{"mx1.conf": "a\n"},
		Message: "mx1",
	}); err != nil {
		t.Fatalf("Commit after falling back to init: %v", err)
	}
}

// Failing to reach the remote must not silently start a divergent history.
func TestOpenFailsWhenRemoteUnreachable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs")
	_, err := Open(context.Background(), repoWithRemote(path, filepath.Join(t.TempDir(), "missing.git")))
	if err == nil {
		t.Fatal("Open must fail rather than initialise a repository that cannot be pushed")
	}
	if !strings.Contains(err.Error(), "clone_on_init") {
		t.Errorf("the error should mention the opt-out, got: %v", err)
	}
}

func TestCloneOnInitDisabled(t *testing.T) {
	no := false
	cfg := repoWithRemote(filepath.Join(t.TempDir(), "configs"), filepath.Join(t.TempDir(), "missing.git"))
	cfg.CloneInit = &no

	if _, err := Open(context.Background(), cfg); err != nil {
		t.Fatalf("with clone_on_init disabled Open must start locally: %v", err)
	}
}

// A volume holding unrelated files is a misconfiguration, not something to
// clone over.
func TestOpenRefusesNonEmptyDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Open(context.Background(), repoWithRemote(path, seedRemote(t)))
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected a refusal to clone over existing files, got: %v", err)
	}
}

// A freshly formatted volume is not "in use" just because ext4 put lost+found
// on it.
func TestOpenIgnoresLostAndFound(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available for the local transport")
	}
	remote := seedRemote(t)

	path := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(filepath.Join(path, "lost+found"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), repoWithRemote(path, remote)); err != nil {
		t.Fatalf("lost+found must not block the clone: %v", err)
	}
}

// Whether a key is required depends on the remote's protocol, so the routing
// is worth pinning down.
func TestRemoteAuthByProtocol(t *testing.T) {
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nnope\n-----END OPENSSH PRIVATE KEY-----"

	tests := []struct {
		name    string
		push    config.Push
		wantErr string
		wantNil bool
	}{
		{
			name:    "local path needs no credentials",
			push:    config.Push{URL: "/srv/git/configs.git"},
			wantNil: true,
		},
		{
			name:    "file:// needs no credentials",
			push:    config.Push{URL: "file:///srv/git/configs.git"},
			wantNil: true,
		},
		{
			name:    "https without credentials falls back to the URL",
			push:    config.Push{URL: "https://git.example.net/noc/configs.git"},
			wantNil: true,
		},
		{
			name: "https with a token",
			push: config.Push{URL: "https://git.example.net/noc/configs.git", Password: "tok"},
		},
		{
			name:    "ssh without a key is rejected",
			push:    config.Push{URL: "git@git.example.net:noc/configs.git"},
			wantErr: "set repo.push.key or repo.push.key_file",
		},
		{
			name:    "ssh:// without a key is rejected",
			push:    config.Push{URL: "ssh://git@git.example.net/noc/configs.git"},
			wantErr: "set repo.push.key or repo.push.key_file",
		},
		{
			name:    "ssh with a malformed inline key reports the key",
			push:    config.Push{URL: "git@git.example.net:noc/configs.git", Key: key},
			wantErr: "push key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := remoteAuth(tc.push)
			switch {
			case tc.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantNil && auth != nil:
				t.Errorf("auth = %v, want none", auth)
			case !tc.wantNil && auth == nil:
				t.Error("auth = nil, want credentials")
			}
		})
	}
}
