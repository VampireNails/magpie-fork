package agent

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func managedWorkBuddySingleLink(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrManagedInvalid
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) != nil || info.NumberOfLinks != 1 {
		return ErrManagedInvalid
	}
	return nil
}
