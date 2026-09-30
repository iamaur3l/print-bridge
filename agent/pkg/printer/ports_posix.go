//go:build !windows

package printer

// LivePortMappings returns no mappings on POSIX systems.
//
// CUPS owns device discovery and its queue names are not pinned to a socket-specific
// port, so there is nothing to follow when hardware moves: the "moved to another
// socket breaks printing" failure is a Windows spooler behaviour. Keeping the same
// signature lets the resolver run cross-platform instead of being Windows-only.
func LivePortMappings() ([]LivePortMapping, error) {
	return nil, nil
}
