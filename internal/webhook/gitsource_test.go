package webhook

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	phelixerr "github.com/abdorrahmani/phelix/internal/errors"
)

// ---------------------------------------------------------------------------
// Real-Git harness
// ---------------------------------------------------------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(gitBinary); err != nil {
		t.Skipf("git is not available: %v", err)
	}
}

// gitMust runs git in dir and fails the test on error.
func gitMust(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git -C %s %s: %v", dir, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out)
}

func gitConfig(t *testing.T, dir string) {
	t.Helper()
	gitMust(t, dir, "config", "user.email", "test@phelix.local")
	gitMust(t, dir, "config", "user.name", "Phelix Test")
}

// gitRepo is a local-origin repository pair: a bare origin plus an app clone
// whose working tree is left at commit A while the remote branch has moved to
// commit B — the exact "webhook arrives while the server tree is behind"
// situation.
type gitRepo struct {
	originDir string
	appDir    string
	commitA   string
	commitB   string
	commitC   string
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	requireGit(t)

	base := t.TempDir()
	work := filepath.Join(base, "origin-work")
	origin := filepath.Join(base, "origin.git")
	app := filepath.Join(base, "app")

	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitMust(t, work, "init", "--initial-branch=main", "-b", "main")
	gitConfig(t, work)
	writeFileT(t, work, "main.go", "package main\n// A\nfunc main() {}\n")
	gitMust(t, work, "add", ".")
	gitMust(t, work, "commit", "-q", "-m", "A")
	commitA := gitMust(t, work, "rev-parse", "HEAD")

	writeFileT(t, work, "main.go", "package main\n// B\nfunc main() {}\n")
	gitMust(t, work, "add", ".")
	gitMust(t, work, "commit", "-q", "-m", "B")
	commitB := gitMust(t, work, "rev-parse", "HEAD")

	writeFileT(t, work, "main.go", "package main\n// C\nfunc main() {}\n")
	gitMust(t, work, "add", ".")
	gitMust(t, work, "commit", "-q", "-m", "C")
	commitC := gitMust(t, work, "rev-parse", "HEAD")

	// Bare origin holding all three commits; the clone stays at A.
	gitMust(t, work, "clone", "-q", "--bare", ".", origin)
	gitMust(t, work, "push", "-q", origin, "main")
	gitMust(t, base, "clone", "-q", origin, app)
	gitConfig(t, app)
	gitMust(t, app, "checkout", "-q", "--detach", commitA)

	return &gitRepo{originDir: origin, appDir: app, commitA: commitA, commitB: commitB, commitC: commitC}
}

func writeFileT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestPreparer(t *testing.T) (SourcePreparer, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "worktrees")
	return NewGitSourcePreparer(root), root
}

func jobFor(repo *gitRepo, commit, delivery string) *Job {
	return &Job{
		AppID:      "app-1",
		AppName:    "api",
		Directory:  repo.appDir,
		Branch:     "main",
		CommitSHA:  commit,
		DeliveryID: delivery,
		Provider:   ProviderGitHub,
		ReceivedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Commit SHA validation (no git needed)
// ---------------------------------------------------------------------------

func TestValidateCommitSHA(t *testing.T) {
	valid := []string{
		"5f0d1c2b7a9e4f8c3d6b1a2c3d4e5f60718293a4",
		strings.Repeat("a", 64), // SHA-256 object id
		strings.ToUpper(strings.Repeat("a", 40)),
	}
	for _, sha := range valid {
		if !ValidateCommitSHA(sha) {
			t.Fatalf("ValidateCommitSHA(%q) = false, want true", sha)
		}
	}
	// Malformed values — including shell-injection attempts — must be
	// rejected BEFORE anything reaches git.
	invalid := []string{
		"",
		"abc",
		"HEAD",
		"HEAD;rm -rf /",
		"$(touch /tmp/phelix-pwned)",
		"refs/heads/main",
		"../../etc",
		strings.Repeat("a", 41),
		strings.Repeat("g", 40),                // not hex
		"5f0d1c2b-7a9e-4f8c-3d6b-1a2c3d4e5f60", // hyphens are not hex
	}
	for _, sha := range invalid {
		if ValidateCommitSHA(sha) {
			t.Fatalf("ValidateCommitSHA(%q) = true, want false", sha)
		}
	}
	if _, err := os.Stat("/tmp/phelix-pwned"); err == nil {
		t.Fatal("command substitution must never have executed")
	}
}

// ---------------------------------------------------------------------------
// Repository contract
// ---------------------------------------------------------------------------

func TestGitSource_RepositoryContract(t *testing.T) {
	t.Run("valid repository", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		top, sub, err := resolveRepository(context.Background(), repo.appDir)
		if err != nil {
			t.Fatalf("resolveRepository: %v", err)
		}
		if top == "" || sub != "" {
			t.Fatalf("toplevel=%q sub=%q, want the clone root with no subpath", top, sub)
		}
	})

	t.Run("non-git directory fails clearly", func(t *testing.T) {
		requireGit(t)
		_, _, err := resolveRepository(context.Background(), t.TempDir())
		if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
			t.Fatalf("err = %v, want GIT_SYNC_FAILED", err)
		}
		if !strings.Contains(err.Error(), "not inside a Git repository") {
			t.Fatalf("error should say why: %v", err)
		}
	})

	t.Run("missing path and file paths fail", func(t *testing.T) {
		requireGit(t)
		f := filepath.Join(t.TempDir(), "afile")
		writeFileT(t, filepath.Dir(f), "afile", "x")
		for _, dir := range []string{filepath.Join(t.TempDir(), "does-not-exist"), f, ""} {
			if _, _, err := resolveRepository(context.Background(), dir); err == nil {
				t.Fatalf("resolveRepository(%q) should fail", dir)
			}
		}
	})

	t.Run("missing remote fails", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		gitMust(t, repo.appDir, "remote", "remove", "origin")
		prep, _ := newTestPreparer(t)
		_, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-noremote"))
		if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
			t.Fatalf("err = %v, want GIT_SYNC_FAILED", err)
		}
		if !strings.Contains(err.Error(), "no remote") {
			t.Fatalf("error should name the missing remote: %v", err)
		}
	})

	t.Run("fetch failure fails the job", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		// Point the remote at a path that cannot be fetched from.
		gitMust(t, repo.appDir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
		prep, _ := newTestPreparer(t)
		_, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-fetchfail"))
		if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
			t.Fatalf("err = %v, want GIT_SYNC_FAILED", err)
		}
		if !strings.Contains(err.Error(), "git fetch failed") {
			t.Fatalf("error should name the fetch failure: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Exact commit preparation
// ---------------------------------------------------------------------------

func TestGitSource_ExactCommit(t *testing.T) {
	t.Run("prepares the requested commit, not the branch tip or local tree", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, root := newTestPreparer(t)

		// The remote branch tip is C, the app working tree is at A — the job
		// asks for B. All three must differ.
		if repo.commitA == repo.commitB || repo.commitB == repo.commitC {
			t.Fatal("test repo commits are not distinct")
		}

		src, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-exact-1"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer src.Cleanup()

		if got := gitMust(t, src.Dir, "rev-parse", "HEAD"); got != repo.commitB {
			t.Fatalf("isolated source HEAD = %s, want the requested commit %s", got, repo.commitB)
		}
		if !strings.HasPrefix(src.Dir, root) {
			t.Fatalf("source %s is outside the managed worktree root %s", src.Dir, root)
		}
		content, err := os.ReadFile(filepath.Join(src.Dir, "main.go"))
		if err != nil || !strings.Contains(string(content), "// B") {
			t.Fatalf("source content is not commit B: %q %v", content, err)
		}
	})

	t.Run("branch HEAD moving after the webhook does not change the target", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)

		// Advance the remote past B (to C) AFTER the delivery for B arrives.
		push := exec.Command(gitBinary, "-C", repo.appDir, "push", "-q", repo.originDir, fmt.Sprintf("%s:refs/heads/moving", repo.commitC))
		if out, err := push.CombinedOutput(); err != nil {
			t.Fatalf("push C: %v: %s", err, out)
		}

		src, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-exact-2"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer src.Cleanup()
		if got := gitMust(t, src.Dir, "rev-parse", "HEAD"); got != repo.commitB {
			t.Fatalf("isolated source HEAD = %s, still want %s (the pushed commit, not any ref)", got, repo.commitB)
		}
	})

	t.Run("commit not found fails clearly", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)
		_, err := prep(context.Background(), jobFor(repo, "e000000000000000000000000000000000000001", "d-notfound"))
		if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
			t.Fatalf("err = %v, want GIT_SYNC_FAILED", err)
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Fatalf("error should say the commit was not found: %v", err)
		}
	})

	t.Run("malformed commit sha rejected before git runs", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)
		for _, sha := range []string{"HEAD;rm -rf /", "$(id)", "", "abc", strings.Repeat("z", 40)} {
			job := jobFor(repo, sha, "d-malformed")
			_, err := prep(context.Background(), job)
			if !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
				t.Fatalf("sha %q: err = %v, want GIT_SYNC_FAILED", sha, err)
			}
			if !strings.Contains(err.Error(), "invalid commit sha") {
				t.Fatalf("sha %q: error should name the invalid sha: %v", sha, err)
			}
		}
	})

	t.Run("unsafe branch value rejected", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)
		pwn := "/tmp/phelix-branch-pwned"
		job := jobFor(repo, repo.commitB, "d-badbranch")
		job.Branch = "main --upload-pack=$(touch " + pwn + ")"
		if _, err := prep(context.Background(), job); !phelixerr.IsCode(err, phelixerr.CodeGitSyncFailed) {
			t.Fatalf("err = %v, want GIT_SYNC_FAILED", err)
		}
		if _, err := os.Stat(pwn); err == nil {
			t.Fatal("branch value must never execute as a command/option")
		}
	})

	t.Run("monorepo subdirectory builds from its worktree counterpart", func(t *testing.T) {
		requireGit(t)
		base := t.TempDir()
		work := filepath.Join(base, "mono-work")
		origin := filepath.Join(base, "origin.git")
		app := filepath.Join(base, "app")
		if err := os.MkdirAll(filepath.Join(work, "services", "api"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitMust(t, work, "init", "-b", "main")
		gitConfig(t, work)
		writeFileT(t, filepath.Join(work, "services", "api"), "main.go", "package main\nfunc main() {}\n")
		gitMust(t, work, "add", ".")
		gitMust(t, work, "commit", "-q", "-m", "mono")
		gitMust(t, work, "clone", "-q", "--bare", ".", origin)
		gitMust(t, work, "push", "-q", origin, "main")
		gitMust(t, base, "clone", "-q", origin, app)

		commit := gitMust(t, app, "rev-parse", "HEAD")
		prep, _ := newTestPreparer(t)
		job := &Job{AppID: "app-1", AppName: "api", Directory: filepath.Join(app, "services", "api"), Branch: "main", CommitSHA: commit, DeliveryID: "d-mono"}
		src, err := prep(context.Background(), job)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer src.Cleanup()
		if !strings.HasSuffix(filepath.ToSlash(src.Dir), "services/api") {
			t.Fatalf("build dir = %s, want the app subdirectory inside the worktree", src.Dir)
		}
		if _, err := os.Stat(filepath.Join(src.Dir, "main.go")); err != nil {
			t.Fatalf("app sources missing in isolated source: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Isolation and cleanup
// ---------------------------------------------------------------------------

func TestGitSource_IsolationAndCleanup(t *testing.T) {
	t.Run("working tree is never mutated", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)

		headBefore := gitMust(t, repo.appDir, "rev-parse", "HEAD")
		writeFileT(t, repo.appDir, "untracked.txt", "user data")
		statusBefore := gitMust(t, repo.appDir, "status", "--porcelain")

		src, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-isolate"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := src.Cleanup(); err != nil {
			t.Fatalf("cleanup: %v", err)
		}

		if got := gitMust(t, repo.appDir, "rev-parse", "HEAD"); got != headBefore {
			t.Fatalf("app working tree HEAD moved: %s -> %s", headBefore, got)
		}
		if got := gitMust(t, repo.appDir, "status", "--porcelain"); got != statusBefore {
			t.Fatalf("app working tree status changed: %q -> %q", statusBefore, got)
		}
		if _, err := os.Stat(filepath.Join(repo.appDir, "untracked.txt")); err != nil {
			t.Fatal("untracked user file was removed from the working tree")
		}
	})

	t.Run("source is removed after cleanup", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)

		src, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-clean"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		dir := src.Dir
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("source should exist before cleanup: %v", err)
		}
		if err := src.Cleanup(); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("source still exists after cleanup: %v", err)
		}
		// The worktree registration must be gone too.
		list := gitMust(t, repo.appDir, "worktree", "list", "--porcelain")
		if strings.Contains(list, dir) {
			t.Fatalf("worktree registration survives cleanup:\n%s", list)
		}
	})

	t.Run("cleanup is idempotent", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)
		src, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-idem"))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if err := src.Cleanup(); err != nil {
			t.Fatalf("first cleanup: %v", err)
		}
		if err := src.Cleanup(); err != nil {
			t.Fatalf("second cleanup: %v", err)
		}
	})

	t.Run("two queued commits never share source state", func(t *testing.T) {
		requireGit(t)
		repo := newGitRepo(t)
		prep, _ := newTestPreparer(t)

		srcA, err := prep(context.Background(), jobFor(repo, repo.commitA, "d-two-a"))
		if err != nil {
			t.Fatalf("prepare A: %v", err)
		}
		srcB, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-two-b"))
		if err != nil {
			t.Fatalf("prepare B: %v", err)
		}
		if srcA.Dir == srcB.Dir {
			t.Fatalf("both commits share the source dir %s", srcA.Dir)
		}
		if got := gitMust(t, srcA.Dir, "rev-parse", "HEAD"); got != repo.commitA {
			t.Fatalf("source A HEAD = %s, want %s", got, repo.commitA)
		}
		if got := gitMust(t, srcB.Dir, "rev-parse", "HEAD"); got != repo.commitB {
			t.Fatalf("source B HEAD = %s, want %s", got, repo.commitB)
		}
		if err := srcA.Cleanup(); err != nil {
			t.Fatalf("cleanup A: %v", err)
		}
		// B must be untouched by A's cleanup.
		if got := gitMust(t, srcB.Dir, "rev-parse", "HEAD"); got != repo.commitB {
			t.Fatalf("source B lost its commit after A's cleanup: %s", got)
		}
		if err := srcB.Cleanup(); err != nil {
			t.Fatalf("cleanup B: %v", err)
		}
	})

	t.Run("different apps prepare concurrently", func(t *testing.T) {
		requireGit(t)
		repoA := newGitRepo(t)
		repoB := newGitRepo(t)
		prep, _ := newTestPreparer(t)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		srcs := make([]*PreparedSource, 2)
		for i, r := range []*gitRepo{repoA, repoB} {
			wg.Add(1)
			go func(i int, r *gitRepo) {
				defer wg.Done()
				srcs[i], errs[i] = prep(context.Background(), jobFor(r, r.commitB, fmt.Sprintf("d-conc-%d", i)))
			}(i, r)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("app %d prepare: %v", i, err)
			}
			defer srcs[i].Cleanup()
		}
		if srcs[0].Dir == srcs[1].Dir {
			t.Fatal("different apps must not share an isolated source")
		}
	})

	t.Run("refuses to remove paths outside the worktree root", func(t *testing.T) {
		g := &gitSourcePreparer{root: filepath.Join(t.TempDir(), "worktrees")}
		if err := g.removeWorktree(t.TempDir(), t.TempDir()); err == nil {
			t.Fatal("removal outside the managed root must be refused")
		}
	})
}

// ---------------------------------------------------------------------------
// Same-app jobs: sequential exact commits through the queue
// ---------------------------------------------------------------------------

func TestQueue_SameAppJobsDeployTheirOwnCommits(t *testing.T) {
	requireGit(t)
	t.Setenv("HOME", t.TempDir())
	repo := newGitRepo(t)
	prep, root := newTestPreparer(t)

	var mu sync.Mutex
	var built []builtSource
	rb := &CliRebuild{
		poll:    5 * time.Millisecond,
		prepare: prep,
		run: func(ctx context.Context, job *Job, dir string, args ...string) ([]string, error) {
			// Capture what the rebuild would compile, at "rebuild" time.
			sourceDir := args[len(args)-1]
			head := gitMust(t, sourceDir, "rev-parse", "HEAD")
			mu.Lock()
			built = append(built, builtSource{dir: sourceDir, head: head, appDir: dir})
			mu.Unlock()
			return nil, nil
		},
	}
	queue := NewQueue(QueueOptions{Rebuild: rb.Rebuild})
	t.Cleanup(func() { queue.Close(2 * time.Second) })

	if err := queue.Enqueue(jobFor(repo, repo.commitA, "job-a")); err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(jobFor(repo, repo.commitB, "job-b")); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(built)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out; %d/2 jobs built", n)
		case <-time.After(5 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := built[0].head, repo.commitA; got != want {
		t.Fatalf("first job built %s, want commit A %s", got, want)
	}
	if got, want := built[1].head, repo.commitB; got != want {
		t.Fatalf("second job built %s, want commit B %s", got, want)
	}
	if built[0].dir == built[1].dir {
		t.Fatal("the two jobs shared one isolated source")
	}
	if !strings.HasPrefix(built[0].dir, root) {
		t.Fatalf("source outside the managed root: %s", built[0].dir)
	}

	// Both isolated sources are cleaned up once the jobs finish.
	waitFor(t, 2*time.Second, func() bool {
		entries, err := os.ReadDir(root)
		return err == nil && len(entries) == 0
	})
}

type builtSource struct {
	dir    string
	head   string
	appDir string
}

// ---------------------------------------------------------------------------
// Startup sweep of abandoned worktrees
// ---------------------------------------------------------------------------

func TestSweepStaleWorktrees(t *testing.T) {
	requireGit(t)
	repo := newGitRepo(t)
	prep, root := newTestPreparer(t)

	stale, err := prep(context.Background(), jobFor(repo, repo.commitB, "d-stale"))
	if err != nil {
		t.Fatalf("prepare stale: %v", err)
	}
	fresh, err := prep(context.Background(), jobFor(repo, repo.commitA, "d-fresh"))
	if err != nil {
		t.Fatalf("prepare fresh: %v", err)
	}

	// Age the first worktree past the staleness threshold.
	old := time.Now().Add(-2 * WorktreeStaleAge)
	worktreeStale := filepath.Dir(filepath.Join(stale.Dir, ".git")) // Dir may be a subpath
	if err := os.Chtimes(worktreeStale, old, old); err != nil {
		// For non-monorepo repos Dir IS the worktree.
		if err := os.Chtimes(stale.Dir, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}

	if err := SweepStaleWorktrees(root); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if _, err := os.Stat(stale.Dir); !os.IsNotExist(err) {
		t.Fatal("stale worktree was not swept")
	}
	if _, err := os.Stat(fresh.Dir); err != nil {
		t.Fatalf("fresh worktree must survive the sweep: %v", err)
	}
	// The swept worktree's registration is pruned from the repository.
	list := gitMust(t, repo.appDir, "worktree", "list", "--porcelain")
	if strings.Contains(list, stale.Dir) {
		t.Fatalf("swept worktree still registered:\n%s", list)
	}
	if !strings.Contains(list, fresh.Dir) {
		t.Fatalf("fresh worktree lost its registration:\n%s", list)
	}
	_ = fresh.Cleanup()
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("condition not met in time")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
