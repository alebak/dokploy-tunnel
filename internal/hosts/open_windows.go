//go:build windows

package hosts

import "os"

// openNoFollow opens path for reading. Windows has no O_NOFOLLOW in the
// standard library; ReadEntriesFile refuses reparse points with Lstat and
// then checks that the opened handle is the same regular file.
func openNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
