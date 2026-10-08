package common

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Data directories and the files in them are owner-only: control.json holds every channel and
// auth instance's signing secret, and the databases hold whatever the app under development put
// in them.
const (
	DataDirPerm  os.FileMode = 0o700
	DataFilePerm os.FileMode = 0o600
)

const dataGitignore = "# altengine emulator data: signing secrets and local databases.\n*\n"

// PrepareDataDir creates dir owner-only and, when it is the emulator's own directory, drops a
// .gitignore in it so its secrets are not committed by a `git add -A`.
//
// "Its own" is a directory this call created, or one named .altengine*: a --data that points at an
// existing folder the developer chose (a project root, say) is not tightened or ignored wholesale.
func PrepareDataDir(dir string) error {
	_, statErr := os.Stat(dir)
	created := errors.Is(statErr, fs.ErrNotExist)
	if err := os.MkdirAll(dir, DataDirPerm); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if !created && !strings.HasPrefix(filepath.Base(abs), ".altengine") {
		return nil
	}
	// A directory an older version created 0755 is tightened in place.
	_ = os.Chmod(dir, DataDirPerm)
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gi, []byte(dataGitignore), DataFilePerm); err != nil {
			return err
		}
	}
	return nil
}
