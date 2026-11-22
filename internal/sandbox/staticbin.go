package sandbox

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
)

// BuildStatic is how to build atl as a static Linux binary, the only kind
// that runs in every task container (InstallBin copies it there).
// -buildmode=exe overrides a GOFLAGS=-buildmode=pie (some distributions set
// it): a position-independent executable needs the system's program
// interpreter, which an image need not have (alpine's is musl's).
const BuildStatic = "CGO_ENABLED=0 go build -buildmode=exe -o ~/.local/bin/atl ./cmd/atl"

// CheckStaticBin returns nil if path is an ELF executable that needs
// nothing from the system to run: no program interpreter (PT_INTERP) and no
// shared library (DT_NEEDED). Otherwise its error says what path is (not
// ELF, e.g. a macOS binary; dynamically linked). It does not compare the
// binary's architecture with anything: the Docker daemon's need not be
// the one atl runs on.
func CheckStaticBin(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		var fe *elf.FormatError
		if errors.As(err, &fe) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%s is not a Linux (ELF) binary", path)
		}
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s does not exist", path)
		}
		return err
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("%s is not an executable (ELF type %v)", path, f.Type)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked (it needs a program interpreter: built with cgo, or as a position-independent executable, e.g. with GOFLAGS=-buildmode=pie)", path)
		}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return fmt.Errorf("%s: read its dynamic section: %w", path, err)
	}
	if len(libs) > 0 {
		return fmt.Errorf("%s is dynamically linked (it needs %s)", path, libs[0])
	}
	return nil
}

// StaticBinError is CheckStaticBin's error as one line for the user: why a
// task container cannot run atl, and how to build it.
func StaticBinError(err error) error {
	return fmt.Errorf("atl runs inside each task's container, so it must be a static Linux binary: %v; build it with `%s` (without Go: from the Dockerfile's build stage, see the README's \"Install on the host\")", err, BuildStatic)
}
