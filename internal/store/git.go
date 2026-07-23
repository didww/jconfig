// Package store persists device configurations in a git repository.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/didww/jconfig/internal/config"
	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// maxAheadWalk bounds the history walk used to count unpushed commits.
const maxAheadWalk = 100000

// Store is a git repository holding one file per device and config format.
// All mutating operations are serialised: git's index is not concurrency safe.
type Store struct {
	cfg config.Repo

	mu   sync.Mutex
	repo *git.Repository
}

// CommitRequest is a set of files to write and commit as a single change.
type CommitRequest struct {
	Device  string
	Files   map[string]string // repo-relative path -> content
	Message string
	When    time.Time
}

// CommitResult describes what a CommitRequest actually did.
type CommitResult struct {
	Changed bool
	Hash    string
	Files   []string
}

// Open opens the repository at cfg.Path, creating it if needed. ctx bounds the
// clone that a missing repository triggers when a push remote is configured.
func Open(ctx context.Context, cfg config.Repo) (*Store, error) {
	if cfg.Path == "" {
		return nil, errors.New("repo.path is empty")
	}
	if err := os.MkdirAll(cfg.Path, 0o755); err != nil {
		return nil, fmt.Errorf("create repo dir: %w", err)
	}

	branch := plumbing.NewBranchReferenceName(cfg.Branch)

	repo, err := git.PlainOpen(cfg.Path)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = createRepo(ctx, cfg, branch)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("open repo %s: %w", cfg.Path, err)
	}

	s := &Store{cfg: cfg, repo: repo}
	if err := s.ensureBranch(branch); err != nil {
		return nil, err
	}
	return s, nil
}

// createRepo populates an empty repo.path. When a push remote is configured it
// clones from it, so that a container starting on a blank volume continues the
// existing history instead of beginning an unrelated one whose first push would
// be rejected as a non-fast-forward. A remote that is empty or does not have
// the branch yet falls back to a fresh repository.
func createRepo(ctx context.Context, cfg config.Repo, branch plumbing.ReferenceName) (*git.Repository, error) {
	initFresh := func() (*git.Repository, error) {
		repo, err := git.PlainInitWithOptions(cfg.Path, &git.PlainInitOptions{
			InitOptions: git.InitOptions{DefaultBranch: branch},
		})
		if err != nil {
			return nil, fmt.Errorf("init repo %s: %w", cfg.Path, err)
		}
		return repo, nil
	}

	if !cfg.CloneOnInit() || cfg.Push.URL == "" {
		return initFresh()
	}
	if err := requireEmptyDir(cfg.Path); err != nil {
		return nil, err
	}

	auth, err := remoteAuth(cfg.Push)
	if err != nil {
		return nil, err
	}

	timeout := cfg.Push.Timeout.Duration()
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	repo, err := git.PlainCloneContext(ctx, cfg.Path, false, &git.CloneOptions{
		URL:           cfg.Push.URL,
		Auth:          auth,
		RemoteName:    cfg.Push.Remote,
		ReferenceName: plumbing.NewBranchReferenceName(cfg.Push.Branch),
		SingleBranch:  true,
	})
	switch {
	case err == nil:
		return repo, nil
	case isRemoteEmpty(err):
		// Nothing to continue from: first ever run against this remote.
		if err := clearDir(cfg.Path); err != nil {
			return nil, err
		}
		return initFresh()
	default:
		return nil, fmt.Errorf("clone %s: %w (set repo.clone_on_init: false to start a local repository instead)",
			cfg.Push.URL, err)
	}
}

// isRemoteEmpty reports whether the clone failed simply because the remote has
// no commits or does not carry the configured branch yet.
func isRemoteEmpty(err error) bool {
	if errors.Is(err, transport.ErrEmptyRemoteRepository) ||
		errors.Is(err, plumbing.ErrReferenceNotFound) ||
		errors.Is(err, git.NoMatchingRefSpecError{}) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "remote repository is empty") ||
		strings.Contains(msg, "couldn't find remote ref")
}

// requireEmptyDir refuses to clone over existing files. lost+found is ignored:
// a freshly formatted ext4 volume always has one.
func requireEmptyDir(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, e := range entries {
		if e.Name() == "lost+found" {
			continue
		}
		return fmt.Errorf("%s is not empty and is not a git repository; "+
			"refusing to clone over it", path)
	}
	return nil
}

// clearDir empties a directory left behind by a failed clone.
func clearDir(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	for _, e := range entries {
		if e.Name() == "lost+found" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(path, e.Name())); err != nil {
			return fmt.Errorf("clean %s: %w", path, err)
		}
	}
	return nil
}

// ensureBranch points HEAD at the configured branch, creating it if the
// repository already has commits on another branch.
func (s *Store) ensureBranch(branch plumbing.ReferenceName) error {
	head, err := s.repo.Head()
	if err != nil {
		// No commits yet: just aim HEAD at the branch.
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return s.repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, branch))
		}
		return fmt.Errorf("read HEAD: %w", err)
	}
	if head.Name() == branch {
		return nil
	}
	wt, err := s.repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	_, refErr := s.repo.Reference(branch, true)
	err = wt.Checkout(&git.CheckoutOptions{
		Branch: branch,
		Create: errors.Is(refErr, plumbing.ErrReferenceNotFound),
		Keep:   true,
	})
	if err != nil {
		return fmt.Errorf("checkout %s: %w", branch.Short(), err)
	}
	return nil
}

// Path returns the repository working directory.
func (s *Store) Path() string { return s.cfg.Path }

// Commit writes the request's files and commits them if anything changed.
// A run that fetched an identical config produces no commit at all.
func (s *Store) Commit(req CommitRequest) (*CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wt, err := s.repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}

	res := &CommitResult{}
	for rel, content := range req.Files {
		changed, err := writeIfChanged(filepath.Join(s.cfg.Path, filepath.FromSlash(rel)), content)
		if err != nil {
			return nil, err
		}
		if changed {
			res.Files = append(res.Files, rel)
		}
		// Add unconditionally: a file may be unchanged on disk but still
		// untracked, for instance after a crash between write and commit.
		if _, err := wt.Add(rel); err != nil {
			return nil, fmt.Errorf("git add %s: %w", rel, err)
		}
	}

	status, err := wt.Status()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	if stagedClean(status, req.Files) {
		return res, nil
	}

	when := req.When
	if when.IsZero() {
		when = time.Now()
	}
	sig := &object.Signature{
		Name:  s.cfg.AuthorName,
		Email: s.cfg.AuthorEmail,
		When:  when,
	}
	hash, err := wt.Commit(req.Message, &git.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}
	res.Changed = true
	res.Hash = hash.String()
	return res, nil
}

func writeIfChanged(path, content string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create dir for %s: %w", path, err)
	}
	old, err := os.ReadFile(path)
	if err == nil && string(old) == content {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}

func stagedClean(status git.Status, files map[string]string) bool {
	for rel := range files {
		if st, ok := status[rel]; ok && st.Staging != git.Unmodified {
			return false
		}
	}
	return true
}

// remoteAuth builds the transport auth for the configured push remote.
func (s *Store) remoteAuth() (transport.AuthMethod, error) { return remoteAuth(s.cfg.Push) }

func remoteAuth(p config.Push) (transport.AuthMethod, error) {
	// Let go-git classify the URL rather than pattern-matching it here: it
	// already understands scp-style git@host:path, ssh://, file:// and bare
	// filesystem paths.
	ep, err := transport.NewEndpoint(p.URL)
	if err != nil {
		return nil, fmt.Errorf("push url %q: %w", p.URL, err)
	}

	switch ep.Protocol {
	case "file":
		// A path on disk needs no credentials.
		return nil, nil

	case "http", "https":
		if p.Username == "" && p.Password == "" {
			return nil, nil // credentials may come from the URL itself
		}
		user := p.Username
		if user == "" {
			user = "git" // token-only auth (GitHub, GitLab)
		}
		return &githttp.BasicAuth{Username: user, Password: p.Password}, nil

	case "ssh":
		return sshRemoteAuth(p, ep.User)

	default:
		return nil, fmt.Errorf("push url %q: unsupported protocol %q", p.URL, ep.Protocol)
	}
}

func sshRemoteAuth(p config.Push, urlUser string) (transport.AuthMethod, error) {
	user := firstNonEmpty(p.Username, urlUser, "git")

	var (
		auth *gitssh.PublicKeys
		err  error
	)
	switch {
	case p.Key != "":
		pem, derr := config.DecodeKey(p.Key)
		if derr != nil {
			return nil, fmt.Errorf("push key: %w", derr)
		}
		auth, err = gitssh.NewPublicKeys(user, pem, p.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("push key: %w", err)
		}
	case p.KeyFile != "":
		auth, err = gitssh.NewPublicKeysFromFile(user, p.KeyFile, p.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("push key_file %s: %w", p.KeyFile, err)
		}
	default:
		return nil, fmt.Errorf("push url %q is an SSH remote: set repo.push.key or repo.push.key_file", p.URL)
	}

	if p.InsecureIgnoreHostKey {
		auth.HostKeyCallback = ssh.InsecureIgnoreHostKey() //nolint:gosec // opt-in
	} else if p.KnownHosts != "" {
		cb, err := gitssh.NewKnownHostsCallback(p.KnownHosts)
		if err != nil {
			return nil, fmt.Errorf("push known_hosts %s: %w", p.KnownHosts, err)
		}
		auth.HostKeyCallback = cb
	}
	return auth, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ensureRemote creates or repoints the push remote.
func (s *Store) ensureRemote() (*git.Remote, error) {
	name, url := s.cfg.Push.Remote, s.cfg.Push.URL

	rem, err := s.repo.Remote(name)
	switch {
	case errors.Is(err, git.ErrRemoteNotFound):
		return s.repo.CreateRemote(&gitconfig.RemoteConfig{Name: name, URLs: []string{url}})
	case err != nil:
		return nil, fmt.Errorf("read remote %s: %w", name, err)
	}
	if len(rem.Config().URLs) == 0 || rem.Config().URLs[0] != url {
		if err := s.repo.DeleteRemote(name); err != nil {
			return nil, fmt.Errorf("update remote %s: %w", name, err)
		}
		return s.repo.CreateRemote(&gitconfig.RemoteConfig{Name: name, URLs: []string{url}})
	}
	return rem, nil
}

// Push mirrors the local branch to the configured remote. It reports
// (false, nil) when the remote was already up to date.
func (s *Store) Push(ctx context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Push.Enabled {
		return false, nil
	}
	if _, err := s.ensureRemote(); err != nil {
		return false, err
	}
	auth, err := s.remoteAuth()
	if err != nil {
		return false, err
	}

	lead := ""
	if s.cfg.Push.Force {
		lead = "+"
	}
	spec := gitconfig.RefSpec(fmt.Sprintf("%s%s:%s", lead,
		plumbing.NewBranchReferenceName(s.cfg.Branch),
		plumbing.NewBranchReferenceName(s.cfg.Push.Branch)))

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Push.Timeout.Duration())
	defer cancel()

	err = s.repo.PushContext(ctx, &git.PushOptions{
		RemoteName: s.cfg.Push.Remote,
		RefSpecs:   []gitconfig.RefSpec{spec},
		Auth:       auth,
		Force:      s.cfg.Push.Force,
	})
	switch {
	case errors.Is(err, git.NoErrAlreadyUpToDate):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("push to %s: %w", s.cfg.Push.Remote, err)
	}
	return true, nil
}

// Unpushed returns the number of commits on the local branch that the remote
// does not have. It contacts the remote, so callers should not poll it hard.
func (s *Store) Unpushed(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.cfg.Push.Enabled {
		return 0, nil
	}
	local, err := s.repo.Reference(plumbing.NewBranchReferenceName(s.cfg.Branch), true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return 0, nil // nothing committed yet
	}
	if err != nil {
		return 0, fmt.Errorf("read local branch: %w", err)
	}

	rem, err := s.ensureRemote()
	if err != nil {
		return 0, err
	}
	auth, err := s.remoteAuth()
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Push.Timeout.Duration())
	defer cancel()

	refs, err := rem.ListContext(ctx, &git.ListOptions{Auth: auth})
	if err != nil {
		if errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return s.countFrom(local.Hash(), plumbing.ZeroHash)
		}
		return 0, fmt.Errorf("list remote %s: %w", s.cfg.Push.Remote, err)
	}

	target := plumbing.NewBranchReferenceName(s.cfg.Push.Branch)
	remoteHash := plumbing.ZeroHash
	for _, r := range refs {
		if r.Name() == target {
			remoteHash = r.Hash()
			break
		}
	}
	if remoteHash == local.Hash() {
		return 0, nil
	}
	return s.countFrom(local.Hash(), remoteHash)
}

// countFrom counts commits reachable from start until stop is seen.
func (s *Store) countFrom(start, stop plumbing.Hash) (int, error) {
	iter, err := s.repo.Log(&git.LogOptions{From: start})
	if err != nil {
		return 0, fmt.Errorf("walk history: %w", err)
	}
	defer iter.Close()

	n := 0
	errStop := errors.New("stop")
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == stop {
			return errStop
		}
		n++
		if n >= maxAheadWalk {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return 0, fmt.Errorf("walk history: %w", err)
	}
	return n, nil
}

// HeadCommit returns the timestamp and hash of the newest commit.
func (s *Store) HeadCommit() (time.Time, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	head, err := s.repo.Head()
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return time.Time{}, "", nil
	}
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read HEAD: %w", err)
	}
	c, err := s.repo.CommitObject(head.Hash())
	if err != nil {
		return time.Time{}, "", fmt.Errorf("read commit: %w", err)
	}
	return c.Committer.When, c.Hash.String(), nil
}

// Dirty reports whether the worktree has uncommitted changes, which means a
// previous run was interrupted between writing and committing.
func (s *Store) Dirty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	wt, err := s.repo.Worktree()
	if err != nil {
		return false, fmt.Errorf("worktree: %w", err)
	}
	status, err := wt.Status()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return !status.IsClean(), nil
}
