package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/khuebt/herdr-runner-kit/internal/project"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func newProjectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Register and manage the projects the kit drives",
		Long: "A project is a git repository the kit keeps a record for. Its runners " +
			"report to the owner project:<name>, so their news reaches the main session " +
			"from any folder.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newProjectRegisterCommand(), newProjectForgetCommand())
	return cmd
}

func newProjectRegisterCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "register <path>",
		Short: "Register an existing git repository as a project",
		Long: "register adds the repository to the kit's registry. It imports the record " +
			"from a committed handbook/product/project.md when there is one, else it starts " +
			"a fresh record. Nothing is written to the repository.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			p, rec, err := project.Register(cmd.Context(), store, args[0], name)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "registered %s at %s (%s, %s track); its runners report to %s\n",
				p.Name, p.Path, rec.Type, rec.Track, task.ProjectOwner(p.Name))
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the committed record's name, else the folder name)")
	return cmd
}

func newProjectForgetCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget <name>",
		Short: "Remove a project from the registry, leaving its repository alone",
		Long: "forget removes the registry entry and the kit's record of the project. It " +
			"refuses while the project has unfinished tasks or changes not yet committed " +
			"to the repository, unless --yes is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := project.RefuseInsideRunner(os.Getenv); err != nil {
				return err
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			if err := project.Forget(store, args[0], yes); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "forgot %s; its repository is unchanged\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "forget even with unfinished tasks or uncommitted changes")
	return cmd
}

// openStore opens the state directory without loading the config.
func openStore() (*state.Store, error) {
	dir, err := stateDir()
	if err != nil {
		return nil, err
	}
	return state.Open(dir)
}

// projectOwners returns the owner of every registered project. A main
// session watches them along with its own, since project runners report
// there whichever folder the session works in.
func projectOwners(s *state.Store) ([]string, error) {
	list, err := s.Projects()
	if err != nil {
		return nil, err
	}
	owners := make([]string, 0, len(list))
	for _, p := range list {
		owners = append(owners, task.ProjectOwner(p.Name))
	}
	return owners, nil
}
