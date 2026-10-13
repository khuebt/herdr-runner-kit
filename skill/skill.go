// Package skill holds the main-agent skill that setup installs into Claude
// Code's skills folder.
package skill

import "embed"

// Name is the skill's folder name under <claude config>/skills.
const Name = "herdr-runner-kit"

// KitOnlyDir holds the project stage templates and the runner preamble. The
// kit renders them; setup does not install them with the skill.
const KitOnlyDir = "references/project"

// Files holds SKILL.md, its references and the kit-only templates.
//
//go:embed SKILL.md references
var Files embed.FS
