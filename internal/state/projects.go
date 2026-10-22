package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The project registry lives next to the tasks:
//
//	<dir>/projects.json            registered projects, guarded by projects.lock
//	<dir>/projects/<name>/         the kit's record and locks of one project

// ErrUnknownProject is returned for a name the registry does not hold.
var ErrUnknownProject = errors.New("unknown project")

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// CheckProjectName refuses a name that is not 1 to 40 lowercase letters,
// digits and dashes, starting with a letter or digit.
func CheckProjectName(name string) error {
	if !projectName.MatchString(name) {
		return fmt.Errorf("invalid project name %q: use 1-40 lowercase letters, digits and dashes, starting with a letter or digit", name)
	}
	return nil
}

// Project is one registry entry. Reserved is set between Reserve and
// Commit, while the project is being set up. WorkspaceID is the herdr
// workspace last opened for the project; herdr reuses ids, so it is a hint
// to check, not a fact.
type Project struct {
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	Reserved     bool      `json:"reserved,omitempty"`
	RegisteredAt time.Time `json:"registeredAt"`
	WorkspaceID  string    `json:"workspaceId,omitempty"`
}

type projectsFile struct {
	SchemaVersion int       `json:"schemaVersion"`
	Projects      []Project `json:"projects"`
}

// ProjectDir returns the folder that holds one project's kit files.
func (s *Store) ProjectDir(name string) string { return filepath.Join(s.dir, "projects", name) }

// LockProject takes the named flock of a project, such as "checkout" or
// "record", creating the project's folder if needed.
func (s *Store) LockProject(name, which string) (func(), error) {
	if err := CheckProjectName(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.ProjectDir(name), dirPerm); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}
	return lock(filepath.Join(s.ProjectDir(name), which+".lock"))
}

// ReserveProject adds name at path as a reserved entry. It refuses a name
// already taken, and a path equal to, inside or above a registered one, so
// two projects never share files. The path must be absolute.
func (s *Store) ReserveProject(name, path string) (Project, error) {
	if err := CheckProjectName(name); err != nil {
		return Project{}, err
	}
	path, err := CanonicalPath(path)
	if err != nil {
		return Project{}, err
	}
	var p Project
	err = s.updateProjects(func(list []Project) ([]Project, error) {
		for _, other := range list {
			if other.Name == name {
				return nil, fmt.Errorf("project %s is already registered at %s", name, other.Path)
			}
			switch {
			case other.Path == path:
				return nil, fmt.Errorf("%s is already registered as project %s", path, other.Name)
			case Within(other.Path, path):
				return nil, fmt.Errorf("%s is inside project %s (%s)", path, other.Name, other.Path)
			case Within(path, other.Path):
				return nil, fmt.Errorf("%s holds project %s (%s)", path, other.Name, other.Path)
			}
		}
		p = Project{Name: name, Path: path, Reserved: true, RegisteredAt: s.now().UTC()}
		return append(list, p), nil
	})
	return p, err
}

// CommitProject turns a reservation into a registered project.
func (s *Store) CommitProject(name string) error {
	return s.updateProjects(func(list []Project) ([]Project, error) {
		i := slices.IndexFunc(list, func(p Project) bool { return p.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrUnknownProject)
		}
		list[i].Reserved = false
		return list, nil
	})
}

// SetProjectWorkspace remembers id as the herdr workspace of project name.
func (s *Store) SetProjectWorkspace(name, id string) error {
	return s.updateProjects(func(list []Project) ([]Project, error) {
		i := slices.IndexFunc(list, func(p Project) bool { return p.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("%s: %w", name, ErrUnknownProject)
		}
		list[i].WorkspaceID = id
		return list, nil
	})
}

// ReleaseProject drops a reservation that was never committed. A committed
// entry is left alone.
func (s *Store) ReleaseProject(name string) error {
	return s.updateProjects(func(list []Project) ([]Project, error) {
		return slices.DeleteFunc(list, func(p Project) bool { return p.Name == name && p.Reserved }), nil
	})
}

// ReleaseStaleReservation drops the reservation of name when it was made
// more than age ago and abandoned reports true for it, and reports whether
// it did. Both checks run under the registry lock, so a reservation another
// process has just made is never dropped. A committed entry is left alone.
func (s *Store) ReleaseStaleReservation(name string, age time.Duration, abandoned func(Project) bool) (bool, error) {
	released := false
	err := s.updateProjects(func(list []Project) ([]Project, error) {
		cutoff := s.now().Add(-age)
		return slices.DeleteFunc(list, func(p Project) bool {
			drop := p.Name == name && p.Reserved && p.RegisteredAt.Before(cutoff) && abandoned(p)
			released = released || drop
			return drop
		}), nil
	})
	return released, err
}

// ForgetProject removes the entry and the project's kit files. The
// repository itself is never touched. It waits for the holders of the
// project's milestone, checkout and record locks.
func (s *Store) ForgetProject(name string) error {
	if err := CheckProjectName(name); err != nil {
		return err
	}
	err := s.updateProjects(func(list []Project) ([]Project, error) {
		n := len(list)
		list = slices.DeleteFunc(list, func(p Project) bool { return p.Name == name })
		if len(list) == n {
			return nil, fmt.Errorf("%s: %w", name, ErrUnknownProject)
		}
		return list, nil
	})
	if err != nil {
		return err
	}
	// The folder holds the project's lock files, so it goes only while
	// this holds them, in lock order: no holder is left on a removed file
	// while the next opener locks a new one.
	for _, which := range []string{"milestone", "checkout", "record"} {
		unlock, err := s.LockProject(name, which)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if err := os.RemoveAll(s.ProjectDir(name)); err != nil {
		return fmt.Errorf("remove project files: %w", err)
	}
	return nil
}

// Projects returns every entry, reserved ones included, ordered by name.
func (s *Store) Projects() ([]Project, error) {
	list, err := s.readProjects()
	if err != nil {
		return nil, err
	}
	slices.SortFunc(list, func(a, b Project) int { return strings.Compare(a.Name, b.Name) })
	return list, nil
}

// ProjectByName returns the entry called name.
func (s *Store) ProjectByName(name string) (Project, error) {
	list, err := s.readProjects()
	if err != nil {
		return Project{}, err
	}
	for _, p := range list {
		if p.Name == name {
			return p, nil
		}
	}
	return Project{}, fmt.Errorf("%s: %w", name, ErrUnknownProject)
}

// ProjectFor returns the committed project whose folder holds dir.
func (s *Store) ProjectFor(dir string) (Project, bool, error) {
	dir, err := CanonicalPath(dir)
	if err != nil {
		return Project{}, false, err
	}
	list, err := s.readProjects()
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range list {
		if !p.Reserved && (p.Path == dir || Within(p.Path, dir)) {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// Within reports whether path lies strictly inside dir. Both are clean
// absolute paths; a sibling that only shares a prefix, such as /a/app2
// next to /a/app, is not inside.
func Within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CanonicalPath returns path as an absolute path with symlinks resolved, as
// git reports top-levels. A path that does not exist yet keeps its missing
// tail under its nearest existing, resolved parent.
func CanonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var tail []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return filepath.Join(append([]string{resolved}, tail...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || dir == filepath.Dir(dir) {
			return "", fmt.Errorf("resolve %s: %w", path, err)
		}
		tail = append([]string{filepath.Base(dir)}, tail...)
	}
}

func (s *Store) projectsPath() string { return filepath.Join(s.dir, "projects.json") }

func (s *Store) readProjects() ([]Project, error) {
	data, err := os.ReadFile(s.projectsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	var f projectsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.projectsPath(), err)
	}
	if f.SchemaVersion > currentSchema {
		return nil, newerSchemaError("projects.json", f.SchemaVersion)
	}
	return f.Projects, nil
}

// updateProjects applies fn to the registry under its lock. If fn returns an
// error, nothing is written.
func (s *Store) updateProjects(fn func([]Project) ([]Project, error)) error {
	unlock, err := lock(filepath.Join(s.dir, "projects.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	list, err := s.readProjects()
	if err != nil {
		return err
	}
	if list, err = fn(list); err != nil {
		return err
	}
	data, err := json.MarshalIndent(projectsFile{SchemaVersion: currentSchema, Projects: list}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode projects: %w", err)
	}
	return writeFileAtomic(s.projectsPath(), append(data, '\n'))
}
