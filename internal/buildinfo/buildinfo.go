// Package buildinfo exposes the product version and the independently
// versioned on-disk and runner contracts.
package buildinfo

// Set at build time with -ldflags "-X github.com/khuebt/herdr-runner-kit/internal/buildinfo.Version=...".
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

const (
	// SchemaVersion is the shape of config and state files. Bump it only with a
	// forward migration.
	SchemaVersion = 1
	// ContractVersion is the report contract runners follow. It stays backward
	// compatible within one major product version.
	ContractVersion = 3
)

// Info is the full build identity.
type Info struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	Date            string `json:"date"`
	SchemaVersion   int    `json:"schemaVersion"`
	ContractVersion int    `json:"contractVersion"`
}

// Get returns the build identity of the running binary.
func Get() Info {
	return Info{
		Version:         Version,
		Commit:          Commit,
		Date:            Date,
		SchemaVersion:   SchemaVersion,
		ContractVersion: ContractVersion,
	}
}
