// Package skill holds the main-agent skill that setup installs into Claude
// Code's skills folder.
package skill

import "embed"

// Name is the skill's folder name under <claude config>/skills.
const Name = "herdr-runner-kit"

// Files holds SKILL.md and its references.
//
//go:embed SKILL.md references
var Files embed.FS
