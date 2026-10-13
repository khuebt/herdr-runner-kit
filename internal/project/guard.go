package project

import "errors"

// ErrInsideRunner is returned when a command that changes a project runs
// inside a runner. Projects change only from the main session, where the
// user sees and approves what happens.
var ErrInsideRunner = errors.New("runners cannot change projects; ask the main session")

// RefuseInsideRunner returns ErrInsideRunner when the caller is a runner,
// which the launcher marks with HRK_TASK. Every command that changes a
// project calls it before anything else.
func RefuseInsideRunner(getenv func(string) string) error {
	if getenv("HRK_TASK") != "" {
		return ErrInsideRunner
	}
	return nil
}
