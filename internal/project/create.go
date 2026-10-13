package project

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/khuebt/herdr-runner-kit/internal/herdr"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// Paths new writes into the repository besides the rendered files.
const (
	GitignorePath = ".gitignore"
	// BriefDir holds the original brief. It is git-ignored and never
	// committed; the intake stage commits a summary instead.
	BriefDir = "handbook/product/discovery/brief/original"
	// IdeaFile is the name of an idea given as text instead of a file.
	IdeaFile = "idea.md"
)

// createMessage is the message of a project's first commit.
const createMessage = "docs: start project"

// staleReservation is how old a reservation must be before new may take
// its name over. new reserves the name first and registers the project at
// the end; a run that was interrupted in between leaves the reservation
// behind. A later new with the same name releases it once it is older than
// this and its folder holds no git repository, so the interrupted run never
// got to set anything up there. Any other reservation stays until
// project forget drops it.
const staleReservation = 10 * time.Minute

// errCredentials is returned for a URL that carries credentials, which
// would end up in .git/config, in output and in the record.
var errCredentials = errors.New("the URL holds credentials; use SSH or a credential helper instead")

// Workspaces opens herdr workspaces. *herdr.Client implements it.
type Workspaces interface {
	WorkspaceLabel(ctx context.Context, id string) (string, bool, error)
	RepoWorkspace(ctx context.Context, root string) (string, error)
	WorkspaceCreate(ctx context.Context, cwd, label string) (herdr.Location, error)
}

// CreateOptions describe a new project. Exactly one of BriefFile and Idea
// is set.
type CreateOptions struct {
	Path          string
	Name          string // default: the folder name
	Remote        string // added as origin when set; never pushed
	Reference     string // a path or URL of a repository to learn from
	InitialBranch string // default: git's init.defaultBranch, else main
	BriefFile     string
	Idea          string
	AllowNonEmpty bool
	// Home is the user's home folder, which is never made a project.
	Home string
}

// Created is what Create made. A step after the first commit that failed
// leaves its field empty and its reason, with the fix, in the matching
// error field.
type Created struct {
	Name, Path, Branch, Commit string
	Remote, RemoteErr          string
	Workspace, WorkspaceErr    string
	RegistryErr                string
	// GitignoreNote is set when the folder already held a .gitignore: new
	// leaves it unchanged and out of the first commit, and the note says
	// how the brief is kept ignored and which line to add.
	GitignoreNote string
	// Brief is the untracked copy of the original brief.
	Brief string
}

// Create turns a folder into a project: a git repository holding a
// .gitignore, the rendered record and an ignored copy of the original
// brief, one first commit of exactly the files it wrote, an optional
// origin, a herdr workspace and a registry entry.
//
// Every check runs before anything is written. Files are created
// exclusively and never through a symbolic link, so nothing that exists is
// overwritten and nothing is written outside the folder. An existing
// .gitignore, with AllowNonEmpty, is left unchanged and out of the commit;
// the brief is ignored through the repository's info/exclude instead. Once
// the repository exists nothing is ever deleted: a failed first commit
// keeps the folder, the brief and the registration, and the error holds
// the command that finishes the commit.
func Create(ctx context.Context, s *state.Store, ws Workspaces, o CreateOptions) (Created, error) {
	p, err := checkCreate(ctx, o)
	if err != nil {
		return Created{}, err
	}
	if _, err := s.ReleaseStaleReservation(p.name, staleReservation, func(old state.Project) bool {
		_, err := os.Lstat(filepath.Join(old.Path, ".git"))
		return errors.Is(err, fs.ErrNotExist)
	}); err != nil {
		return Created{}, err
	}
	if _, err := s.ReserveProject(p.name, p.path); err != nil {
		return Created{}, err
	}
	rec := NewRecord(p.name)
	rec.ReferenceRepo = p.reference
	rec.Decisions = []Decision{{At: time.Now().UTC(), Text: "project created (herdr-runner-kit new)"}}
	rec.Pending = 1
	if err := Save(s, rec); err != nil {
		return Created{}, errors.Join(err, s.ForgetProject(p.name))
	}
	if err := initRepo(ctx, p); err != nil {
		return Created{}, errors.Join(err, s.ForgetProject(p.name))
	}

	// The repository exists: from here on the project stays registered,
	// so status shows what is missing.
	c := Created{Name: p.name, Path: p.path, Branch: p.branch, Brief: filepath.Join(p.path, BriefDir, p.briefName)}
	keep := func(err error) (Created, error) {
		if rerr := s.CommitProject(p.name); rerr != nil {
			err = errors.Join(err, fmt.Errorf("register %s: %w", p.name, rerr))
		}
		return Created{}, fmt.Errorf("%w\nnothing was deleted: the folder %s and the original brief %s are kept, and %s stays registered",
			err, p.path, c.Brief, p.name)
	}
	if err := writeFiles(ctx, p, o, rec); err != nil {
		return keep(err)
	}
	if c.Commit, err = firstCommit(ctx, s, p); err != nil {
		return keep(err)
	}
	if _, err := update(s, p.name, func(r *Record) error {
		r.Pending = 0
		return nil
	}); err != nil {
		return keep(fmt.Errorf("committed %s, but could not update the record: %w", c.Commit, err))
	}

	if o.Remote != "" {
		if out, err := gitCombined(ctx, p.path, "remote", "add", "origin", o.Remote); err != nil {
			c.RemoteErr = fmt.Sprintf("%s; add it with: git -C %s remote add origin %s",
				RedactURL(firstLine(out, err)), shellCommand([]string{p.path}), RedactURL(o.Remote))
		} else {
			c.Remote = RedactURL(o.Remote)
		}
	}
	if c.Workspace, err = openWorkspace(ctx, s, ws, p); err != nil {
		c.WorkspaceErr = fmt.Sprintf("%s; project runners open it on their first launch, or open it with: herdr workspace create --cwd %s --label %s",
			err, shellCommand([]string{p.path}), p.name)
	}
	if p.gitignoreExists {
		c.GitignoreNote = fmt.Sprintf("your %s is unchanged and not in the first commit; the brief is ignored through .git/info/exclude, so to keep it ignored in every clone add this line to %s and commit it: %s",
			GitignorePath, GitignorePath, ignoreLine)
	}
	if err := s.CommitProject(p.name); err != nil {
		c.RegistryErr = fmt.Sprintf("%s; register it with: herdr-runner-kit project forget %s --yes && herdr-runner-kit project register %s",
			err, p.name, shellCommand([]string{p.path}))
	}
	return c, nil
}

// plan holds the checked inputs of Create.
type plan struct {
	path, name, branch, reference string
	briefName                     string
	gitignoreExists               bool
}

// checkCreate refuses every input Create must not act on.
func checkCreate(ctx context.Context, o CreateOptions) (plan, error) {
	var p plan
	switch {
	case o.BriefFile != "" && o.Idea != "":
		return p, errors.New("give either a brief file or an idea, not both")
	case o.BriefFile == "" && strings.TrimSpace(o.Idea) == "":
		return p, errors.New("a new project needs a brief file or an idea")
	}
	if strings.TrimSpace(o.Path) == "" {
		return p, errors.New("a new project needs a folder")
	}
	if o.Remote != "" {
		if err := CheckURL(o.Remote); err != nil {
			return p, fmt.Errorf("remote: %w", err)
		}
	}
	ref, err := checkReference(o.Reference)
	if err != nil {
		return p, err
	}
	p.reference = ref
	if p.path, err = state.CanonicalPath(o.Path); err != nil {
		return p, err
	}
	if err := checkPlace(p.path, o.Home); err != nil {
		return p, err
	}
	if p.gitignoreExists, err = checkFolder(p.path, o.AllowNonEmpty); err != nil {
		return p, err
	}
	parent := nearestExisting(p.path)
	if top, inside := insideRepo(ctx, parent); inside {
		return p, fmt.Errorf("%s is inside the git repository %s; pick a folder outside it", p.path, top)
	}

	p.name = o.Name
	if p.name == "" {
		p.name = strings.ToLower(filepath.Base(p.path))
		if state.CheckProjectName(p.name) != nil {
			return p, fmt.Errorf("folder name %q is not a valid project name; pass --name", filepath.Base(p.path))
		}
	}
	if err := state.CheckProjectName(p.name); err != nil {
		return p, err
	}

	p.briefName = IdeaFile
	if o.BriefFile != "" {
		info, err := os.Stat(o.BriefFile)
		if err != nil {
			return p, fmt.Errorf("brief: %w", err)
		}
		if !info.Mode().IsRegular() {
			return p, fmt.Errorf("brief %s is not a regular file", o.BriefFile)
		}
		p.briefName = filepath.Base(o.BriefFile)
	}
	written := []string{SnapshotPath, DecisionsPath, filepath.Join(BriefDir, p.briefName)}
	if !p.gitignoreExists {
		written = append(written, GitignorePath)
	}
	for _, rel := range written {
		if _, err := os.Lstat(filepath.Join(p.path, rel)); !errors.Is(err, fs.ErrNotExist) {
			return p, fmt.Errorf("%s already exists in %s; new never overwrites a file", rel, p.path)
		}
		if err := checkParents(p.path, rel); err != nil {
			return p, err
		}
	}

	p.branch = o.InitialBranch
	if p.branch == "" {
		if p.branch, _ = git(ctx, parent, "config", "--get", "init.defaultBranch"); p.branch == "" {
			p.branch = "main"
		}
	}
	if _, err := git(ctx, parent, "check-ref-format", "--branch", p.branch); err != nil || strings.HasPrefix(p.branch, "-") {
		return p, fmt.Errorf("%q is not a valid branch name", p.branch)
	}
	return p, nil
}

// checkPlace refuses the filesystem root, the home folder and any folder
// above it.
func checkPlace(path, home string) error {
	if path == filepath.Dir(path) {
		return fmt.Errorf("%s is the filesystem root; pick a folder for the project", path)
	}
	if home == "" {
		return nil
	}
	home, err := state.CanonicalPath(home)
	if err != nil {
		return err
	}
	if path == home || state.Within(path, home) {
		return fmt.Errorf("%s holds your home folder; pick a folder for the project", path)
	}
	return nil
}

// checkFolder refuses a path that is not a folder, already a repository,
// non-empty without allowNonEmpty, or holding a repository below it. It
// reports whether the folder holds a .gitignore.
func checkFolder(path string, allowNonEmpty bool) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%s exists and is not a folder", path)
	}
	if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
		return false, fmt.Errorf("%s is already a git repository; use herdr-runner-kit project register", path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	if len(entries) == 0 {
		return false, nil
	}
	if !allowNonEmpty {
		return false, fmt.Errorf("%s is not empty; pass --allow-non-empty to start a project in it", path)
	}
	err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			return fmt.Errorf("%s holds the git repository %s; pick a folder that does not", path, filepath.Dir(p))
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	info, err = os.Lstat(filepath.Join(path, GitignorePath))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("%s in %s is not a regular file; new reads it only when it is one", GitignorePath, path)
	}
	return true, nil
}

// checkParents refuses a file path rel under root whose existing folders
// include a symbolic link or something that is not a folder, so a write
// never lands outside root.
func checkParents(root, rel string) error {
	dir := root
	for _, part := range strings.Split(filepath.Dir(rel), string(filepath.Separator)) {
		if part == "." {
			continue
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return nil
		case err != nil:
			return err
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symbolic link; new writes only inside %s and never through a link", dir, root)
		case !info.IsDir():
			return fmt.Errorf("%s is not a folder, so new cannot write %s", dir, rel)
		}
	}
	return nil
}

// nearestExisting returns path, or its nearest parent that exists.
func nearestExisting(path string) string {
	for {
		if _, err := os.Stat(path); err == nil || path == filepath.Dir(path) {
			return path
		}
		path = filepath.Dir(path)
	}
}

// insideRepo reports whether dir lies in a git repository, and its
// top-level when it has one.
func insideRepo(ctx context.Context, dir string) (string, bool) {
	if _, err := git(ctx, dir, "rev-parse", "--git-dir"); err != nil {
		return "", false
	}
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		top = dir
	}
	return top, true
}

// checkReference returns the reference repository as it is stored: a URL
// as given, a path made absolute.
func checkReference(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if isURL(ref) {
		if err := CheckURL(ref); err != nil {
			return "", fmt.Errorf("reference: %w", err)
		}
		return ref, nil
	}
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", fmt.Errorf("reference: %w", err)
	}
	return abs, nil
}

// isURL reports whether s is a URL or an scp-like address such as
// git@host:repo, rather than a local path.
func isURL(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	colon := strings.Index(s, ":")
	slash := strings.Index(s, "/")
	return colon > 0 && (slash < 0 || colon < slash)
}

// CheckURL refuses a git URL that holds credentials: a password, or a
// user or token in the authority of anything but SSH, where the user is
// part of the address.
func CheckURL(raw string) error {
	if raw == "" || strings.HasPrefix(raw, "-") || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return errors.New("not a valid git URL")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			// url.Parse quotes the URL in its error; it may hold a secret.
			return errors.New("not a valid git URL")
		}
		if u.User == nil {
			return nil
		}
		if _, hasPassword := u.User.Password(); hasPassword || !sshScheme(u.Scheme) {
			return errCredentials
		}
		return nil
	}
	if user, ok := scpUser(raw); ok && strings.Contains(user, ":") {
		return errCredentials
	}
	return nil
}

// RedactURL returns a git URL without the credentials CheckURL refuses, so
// it can be printed or stored. Text that is not a URL passes unchanged
// except for any credentials embedded in URLs inside it.
func RedactURL(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) != 1 || fields[0] != raw {
		for _, f := range fields {
			if r := RedactURL(f); r != f {
				raw = strings.ReplaceAll(raw, f, r)
			}
		}
		return raw
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			if strings.Contains(raw, "@") {
				return "<URL hidden>"
			}
			return raw
		}
		if u.User == nil {
			return raw
		}
		if _, hasPassword := u.User.Password(); !hasPassword && sshScheme(u.Scheme) {
			return raw
		}
		u.User = nil
		return u.String()
	}
	if user, ok := scpUser(raw); ok && strings.Contains(user, ":") {
		return strings.TrimPrefix(raw, user+"@")
	}
	return raw
}

func sshScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "ssh", "git+ssh", "ssh+git":
		return true
	}
	return false
}

// scpUser returns the user part of an scp-like address user@host:path.
func scpUser(raw string) (string, bool) {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return "", false
	}
	if slash := strings.Index(raw, "/"); slash >= 0 && slash < at {
		return "", false
	}
	return raw[:at], true
}

// initRepo creates the folder and its repository.
func initRepo(ctx context.Context, p plan) error {
	if err := os.MkdirAll(p.path, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", p.path, err)
	}
	if out, err := gitCombined(ctx, p.path, "init", "-q", "--initial-branch="+p.branch); err != nil {
		return fmt.Errorf("git init: %s", firstLine(out, err))
	}
	return nil
}

// ignoreLine keeps the original brief out of git.
const ignoreLine = BriefDir + "/"

// writeFiles writes the .gitignore, the original brief and the rendered
// record, each created exclusively, and checks that git ignores the brief.
func writeFiles(ctx context.Context, p plan, o CreateOptions, rec Record) error {
	if err := writeIgnore(p); err != nil {
		return err
	}
	brief := []byte(strings.TrimSpace(o.Idea) + "\n")
	if o.BriefFile != "" {
		data, err := readBrief(o.BriefFile)
		if err != nil {
			return err
		}
		brief = data
	}
	briefRel := filepath.Join(BriefDir, p.briefName)
	if err := writeNew(p.path, briefRel, brief, 0o600); err != nil {
		return err
	}
	if err := checkIgnored(ctx, p.path, briefRel); err != nil {
		return err
	}
	for _, rel := range []string{SnapshotPath, DecisionsPath} {
		data, err := Render(rec, rel)
		if err != nil {
			return err
		}
		if err := writeNew(p.path, rel, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeIgnore keeps the brief out of git: in a new .gitignore, or, when the
// folder already holds one, in the repository's info/exclude, so the
// user's file stays as it is.
func writeIgnore(p plan) error {
	if !p.gitignoreExists {
		return writeNew(p.path, GitignorePath, []byte(ignoreLine+"\n"), 0o644)
	}
	// initRepo just made .git, so info/exclude is the kit's own file.
	exclude := filepath.Join(p.path, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(exclude), err)
	}
	data, err := os.ReadFile(exclude)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", exclude, err)
	}
	add := ignoreLine + "\n"
	if len(data) > 0 && data[len(data)-1] != '\n' {
		add = "\n" + add
	}
	f, err := os.OpenFile(exclude, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", exclude, err)
	}
	_, err = f.WriteString(add)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("append to %s: %w", exclude, err)
	}
	return nil
}

// checkIgnored fails unless git ignores rel in root, so the original brief
// can never reach a commit.
func checkIgnored(ctx context.Context, root, rel string) error {
	err := exec.CommandContext(ctx, "git", "-C", root, "check-ignore", "-q", "--", rel).Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return fmt.Errorf("git does not ignore the original brief %s: a rule in the folder's ignore files lets it in; fix the rule so git check-ignore %s succeeds, then commit the record",
			rel, shellCommand([]string{rel}))
	}
	return fmt.Errorf("git check-ignore: %w", err)
}

func readBrief(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("brief: %w", err)
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("brief: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("brief %s is not a regular file", path)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read brief: %w", err)
	}
	return data, nil
}

// writeNew creates rel under root with data, refusing to replace an
// existing file, to follow a symbolic link, or to write outside root.
func writeNew(root, rel string, data []byte, mode os.FileMode) error {
	path := filepath.Join(root, rel)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(dir); err != nil {
		return fmt.Errorf("resolve %s: %w", dir, err)
	} else if resolved != root && !state.Within(root, resolved) {
		return fmt.Errorf("%s leads outside %s; new writes only inside the folder", dir, root)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// firstCommit commits exactly the files Create wrote, under the project's
// checkout lock.
func firstCommit(ctx context.Context, s *state.Store, p plan) (string, error) {
	unlock, err := s.LockProject(p.name, "checkout")
	if err != nil {
		return "", err
	}
	defer unlock()
	paths := []string{SnapshotPath, DecisionsPath}
	if !p.gitignoreExists {
		paths = append([]string{GitignorePath}, paths...)
	}
	if out, err := gitLocked(ctx, p.path, append([]string{"add", "--"}, paths...)...); err != nil {
		return "", fmt.Errorf("git add: %s\ncommit the files with:\n  %s", firstLine(out, err),
			shellCommand(append([]string{"git", "-C", p.path, "add", "--"}, paths...))+" && "+
				shellCommand(append([]string{"git", "-C", p.path, "commit", "-m", createMessage, "--"}, paths...)))
	}
	return commitPaths(ctx, p.path, createMessage, paths)
}

// openWorkspace returns the project's open herdr workspace, opening one
// when there is none.
func openWorkspace(ctx context.Context, s *state.Store, ws Workspaces, p plan) (string, error) {
	if ws == nil {
		return "", errors.New("herdr is not available")
	}
	return OpenWorkspace(ctx, s, ws, p.name, p.path)
}

// OpenWorkspace returns the herdr workspace of project name, whose
// repository is at root, and remembers it in the registry. herdr does not
// report the folder of a workspace made with workspace create, so the
// remembered id is tried first; it counts only while herdr still has it
// under the project's label, since herdr reuses ids. Next comes the
// repository's open workspace, and last a new one. The project's workspace
// lock keeps two callers from opening it twice.
func OpenWorkspace(ctx context.Context, s *state.Store, ws Workspaces, name, root string) (string, error) {
	unlock, err := s.LockProject(name, "ws")
	if err != nil {
		return "", err
	}
	defer unlock()
	p, err := s.ProjectByName(name)
	if err != nil {
		return "", err
	}
	if p.WorkspaceID != "" {
		label, found, err := ws.WorkspaceLabel(ctx, p.WorkspaceID)
		if err != nil {
			return "", err
		}
		if found && label == name {
			return p.WorkspaceID, nil
		}
	}
	id, err := ws.RepoWorkspace(ctx, root)
	if err != nil {
		return "", err
	}
	if id == "" {
		loc, err := ws.WorkspaceCreate(ctx, root, name)
		if err != nil {
			return "", err
		}
		if loc.WorkspaceID == "" {
			return "", errors.New("herdr workspace create returned no workspace id")
		}
		id = loc.WorkspaceID
	}
	if id != p.WorkspaceID {
		if err := s.SetProjectWorkspace(name, id); err != nil {
			return "", fmt.Errorf("remember workspace %s of project %s: %w", id, name, err)
		}
	}
	return id, nil
}

var _ Workspaces = (*herdr.Client)(nil)
