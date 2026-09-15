package webhook

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
	"github.com/abdorrahmani/phelix/internal/logs"
)

const (
	// DefaultWorktreeRootName is the directory (under <dataDir>/webhook/)
	// that holds the temporary isolated sources.
	DefaultWorktreeRootName = "worktrees"

	// WorktreeStaleAge bounds how long an abandoned worktree may survive
	// before the startup sweep removes it. A worktree whose owning daemon
	// died is not removed immediately: the rebuild subprocess it fed may
	// still be running as an orphan, and one hour comfortably exceeds any
	// build while still bounding the leak.
	WorktreeStaleAge = time.Hour
)

// gitBinary is the executable every Git operation uses. A var so tests can
// verify the availability check.
var gitBinary = "git"

// runGit executes git with structured arguments in dir — never through a
// shell, so no value (branch, commit, remote, path) can ever be interpreted
// as shell syntax. Output is captured for the caller; stderr is kept out of
// error messages that carry payload-derived values.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, gitBinary, full...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Preserve cancellation in the chain so a shutdown mid-fetch is
		// classified as a shutdown, not a Git failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return stdout.String(), phelixerr.Wrap(phelixerr.CodeUnavailable, "git command cancelled", ctxErr)
		}
		// The git stderr itself is safe to include (it names refs and paths
		// of this repository, never credentials); trim it to keep logs tight.
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

// ValidateCommitSHA reports whether sha is a well-formed Git object id
// (40-char SHA-1 or 64-char SHA-256, lowercase or uppercase hex). It must be
// checked BEFORE the value is passed to any Git command so a payload value
// can never reach Git as a flag or revision expression.
func ValidateCommitSHA(sha string) bool {
	if len(sha) != 40 && len(sha) != 64 {
		return false
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// isSafeGitArg rejects values that Git could parse as options (leading dash)
// or that contain path/shell metacharacters. Git never runs through a shell
// here; this is defense in depth for argument confusion.
func isSafeGitArg(v string) bool {
	if v == "" || strings.HasPrefix(v, "-") {
		return false
	}
	return !strings.ContainsAny(v, " \t\r\n\x00;|&$`\"'<>\\*?[")
}

// PreparedSource is an isolated source tree checked out at an exact commit.
// Dir is the directory the build must consume (the worktree, or a subdirectory
// of it when the app lives in a monorepo). Cleanup removes the worktree; it
// is idempotent and must be called on every path (success, build failure,
// deploy failure, cancellation).
type PreparedSource struct {
	Dir         string
	repoDir     string
	worktreeDir string

	cleanupOnce sync.Once
	cleanupErr  error
	remove      func() error
}

// Cleanup removes the isolated worktree. It never touches the application's
// real repository or working tree. Failures are returned (the caller logs
// them) and must never mask the deployment's own outcome; the fallback path
// removes the directory directly and prunes stale worktree registrations.
func (p *PreparedSource) Cleanup() error {
	if p == nil || p.remove == nil {
		return nil
	}
	p.cleanupOnce.Do(func() { p.cleanupErr = p.remove() })
	return p.cleanupErr
}

// SourcePreparer prepares the isolated exact-commit source for a job. It is
// the Phase 2 stage between the queue and `phelix rebuild`: on failure the
// rebuild must not run at all.
type SourcePreparer func(ctx context.Context, job *Job) (*PreparedSource, error)

// gitSourcePreparer fetches and checks out the job's pushed commit into a
// temporary worktree under root.
type gitSourcePreparer struct {
	root string
}

// NewGitSourcePreparer returns the production SourcePreparer. root is the
// directory that holds (and gets swept of) temporary worktrees.
func NewGitSourcePreparer(root string) SourcePreparer {
	return (&gitSourcePreparer{root: root}).prepare
}

// prepare implements the exact-commit source contract:
//
//  1. validate the commit SHA (before it reaches Git);
//  2. validate the app directory is a Git repository;
//  3. fetch the configured remote (by branch; the pushed commit is reachable
//     from the remote branch tip);
//  4. verify the exact commit exists locally (retrying with a direct
//     SHA fetch for servers that allow it);
//  5. check out the exact commit into a fresh detached worktree — the
//     application's working tree is never touched;
//  6. verify the worktree HEAD is exactly the requested commit.
//
// The main working tree's branch, HEAD and uncommitted changes are never
// modified, and the branch having moved on the remote in the meantime does
// not matter: the deployment target is the SHA, not a ref.
func (g *gitSourcePreparer) prepare(ctx context.Context, job *Job) (*PreparedSource, error) {
	if !ValidateCommitSHA(job.CommitSHA) {
		return nil, phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: git source for app %s: invalid commit sha %q", job.AppName, job.CommitSHA)
	}
	if !isSafeGitArg(job.Branch) {
		return nil, phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: git source for app %s: unsafe branch value %q", job.AppName, job.Branch)
	}

	repoDir, subPath, err := resolveRepository(ctx, job.Directory)
	if err != nil {
		return nil, err
	}

	remote, err := resolveRemote(ctx, repoDir)
	if err != nil {
		return nil, err
	}

	// Fetch the branch (not a pull: the working tree must not move). Fetch
	// output can embed remote URLs (which may carry credentials), so only
	// the remote's NAME is logged.
	if _, err := runGit(ctx, repoDir, "fetch", "--quiet", remote, job.Branch); err != nil {
		return nil, phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: git fetch failed for app %s (remote %q, branch %q)", job.AppName, remote, job.Branch)
	}

	if err := ensureCommitPresent(ctx, repoDir, remote, job.CommitSHA); err != nil {
		return nil, err
	}

	worktree, err := g.addWorktree(ctx, repoDir, job)
	if err != nil {
		return nil, err
	}

	// The worktree must be EXACTLY the requested commit — never the branch's
	// current tip, which may have moved since the push.
	head, err := runGit(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		g.removeWorktree(repoDir, worktree)
		return nil, phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: could not verify worktree HEAD for app %s", job.AppName)
	}
	if got := strings.TrimSpace(head); !strings.EqualFold(got, job.CommitSHA) {
		g.removeWorktree(repoDir, worktree)
		return nil, phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: worktree HEAD %s is not the requested commit %s for app %s", got, job.CommitSHA, job.AppName)
	}

	// An app configured in a subdirectory of a monorepo builds from the same
	// subdirectory inside the worktree.
	buildDir := worktree
	if subPath != "" {
		buildDir = filepath.Join(worktree, subPath)
		if st, err := os.Stat(buildDir); err != nil || !st.IsDir() {
			g.removeWorktree(repoDir, worktree)
			return nil, phelixerr.Newf(phelixerr.CodeGitSyncFailed,
				"webhook: commit %s does not contain the app directory %q for app %s", job.CommitSHA, subPath, job.AppName)
		}
	}

	src := &PreparedSource{
		Dir:         buildDir,
		repoDir:     repoDir,
		worktreeDir: worktree,
	}
	src.remove = func() error { return g.removeWorktree(repoDir, worktree) }
	return src, nil
}

// resolveRepository verifies dir is inside a Git working tree and returns the
// repository toplevel plus the app's path within it ("" when the app IS the
// toplevel).
func resolveRepository(ctx context.Context, dir string) (toplevel, subPath string, err error) {
	if dir == "" {
		return "", "", phelixerr.New(phelixerr.CodeGitSyncFailed, "webhook: application has no source directory")
	}
	if _, err := exec.LookPath(gitBinary); err != nil {
		return "", "", phelixerr.Wrap(phelixerr.CodeGitSyncFailed, "git executable not found", err)
	}
	top, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: %s is not inside a Git repository — the webhook deploys the pushed commit, which requires a repository", dir)
	}
	toplevel = strings.TrimSpace(top)
	if toplevel == "" {
		return "", "", phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: %s is not inside a Git repository", dir)
	}
	prefix, err := runGit(ctx, dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", "", phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: could not resolve the app's position inside the repository at %s", toplevel)
	}
	return toplevel, strings.Trim(strings.TrimSpace(prefix), "/"), nil
}

// resolveRemote returns the remote to fetch from: "origin" when configured,
// otherwise the single configured remote. Repositories without a usable
// remote fail clearly.
func resolveRemote(ctx context.Context, repoDir string) (string, error) {
	out, err := runGit(ctx, repoDir, "remote")
	if err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: could not list Git remotes for %s", repoDir)
	}
	var remotes []string
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			remotes = append(remotes, name)
		}
	}
	for _, name := range remotes {
		if name == "origin" && isSafeGitArg(name) {
			return name, nil
		}
	}
	if len(remotes) == 1 && isSafeGitArg(remotes[0]) {
		return remotes[0], nil
	}
	return "", phelixerr.Newf(phelixerr.CodeGitSyncFailed,
		"webhook: repository %s has no remote to fetch the pushed commit from (expected an %q remote)", repoDir, "origin")
}

// ensureCommitPresent verifies the exact commit exists locally after the
// branch fetch. When it does not (e.g. the branch was force-pushed past it,
// or the push is on a ref the fetch did not cover), a direct SHA fetch is
// attempted — allowed by servers with uploadpack.allowAnySHA1InWant.
func ensureCommitPresent(ctx context.Context, repoDir, remote, sha string) error {
	if _, err := runGit(ctx, repoDir, "cat-file", "-e", sha+"^{commit}"); err == nil {
		return nil
	}
	if _, err := runGit(ctx, repoDir, "fetch", "--quiet", remote, sha); err != nil {
		return phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: commit %s not found after fetching — it may have been force-pushed away on the remote", sha)
	}
	if _, err := runGit(ctx, repoDir, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: commit %s not found after fetching", sha)
	}
	return nil
}

// addWorktree checks the exact commit out into a fresh detached worktree with
// a unique name, so two queued commits can never share source state.
func (g *gitSourcePreparer) addWorktree(ctx context.Context, repoDir string, job *Job) (string, error) {
	if err := os.MkdirAll(g.root, 0o755); err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeGitSyncFailed, "webhook: create worktree root", err)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return "", phelixerr.Wrap(phelixerr.CodeGitSyncFailed, "webhook: generate worktree name", err)
	}
	name := fmt.Sprintf("%s-%s-%s", sanitizePathSegment(job.AppName), sanitizePathSegment(job.DeliveryID), suffix)
	worktree := filepath.Join(g.root, name)
	if _, err := runGit(ctx, repoDir, "worktree", "add", "--detach", "--quiet", worktree, job.CommitSHA); err != nil {
		return "", phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: could not create an isolated source at commit %s for app %s", job.CommitSHA, job.AppName)
	}
	return worktree, nil
}

// removeWorktree removes one isolated worktree and its registration. It never
// deletes anything outside the worktree root.
func (g *gitSourcePreparer) removeWorktree(repoDir, worktree string) error {
	if worktree == "" || !strings.HasPrefix(filepath.Clean(worktree), filepath.Clean(g.root)+string(os.PathSeparator)) {
		// Never remove anything that is not under the managed root; the
		// application's real repository can never be passed here.
		return phelixerr.Newf(phelixerr.CodeGitSyncFailed,
			"webhook: refusing to remove worktree outside %s", g.root)
	}
	if _, err := runGit(context.Background(), repoDir, "worktree", "remove", "--force", worktree); err == nil {
		return nil
	}
	// Fallback: drop the directory, then prune the stale registration.
	if err := os.RemoveAll(worktree); err != nil {
		return phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err,
			"webhook: could not remove isolated source %s", worktree)
	}
	_, _ = runGit(context.Background(), repoDir, "worktree", "prune")
	return nil
}

// sanitizePathSegment makes an app name or delivery id safe to use as one
// path segment of a worktree name: keep alphanumerics and . _ -, collapse
// everything else, and bound the length.
func sanitizePathSegment(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// SweepStaleWorktrees removes abandoned worktrees under root (created by a
// previous daemon run that died before cleanup) and prunes their
// registrations in the owning repositories. Entries modified within
// WorktreeStaleAge are left alone: their rebuild may still be running as an
// orphaned subprocess.
func SweepStaleWorktrees(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return phelixerr.Wrapf(phelixerr.CodeGitSyncFailed, err, "webhook: sweep worktree root %s", root)
	}
	cutoff := time.Now().Add(-WorktreeStaleAge)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		repo := repoForWorktree(dir)
		if err := os.RemoveAll(dir); err != nil {
			logs.Warning("webhook", "sweep: could not remove stale worktree %s: %v", dir, err)
			continue
		}
		if repo != "" {
			// Prune the now-dangling registration in the owning repository.
			// Safe for live worktrees: prune only drops registrations whose
			// directories no longer exist.
			if _, err := runGit(context.Background(), repo, "worktree", "prune"); err != nil {
				logs.Warning("webhook", "sweep: could not prune worktree registration in %s: %v", repo, err)
			}
		}
		logs.Info("webhook", "sweep: removed stale isolated source %s", dir)
	}
	return nil
}

// repoForWorktree locates the repository that owns a worktree by parsing its
// .git pointer file ("gitdir: <repo>/.git/worktrees/<id>"). Returns "" when
// the pointer cannot be resolved; the caller then just removes the directory.
func repoForWorktree(worktree string) string {
	data, err := os.ReadFile(filepath.Join(worktree, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	// <repo>/.git/worktrees/<id> → <repo>
	parent := filepath.Dir(filepath.Dir(gitdir))
	if parent == "." || parent == string(filepath.Separator) {
		return ""
	}
	return parent
}
