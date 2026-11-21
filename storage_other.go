//go:build !linux

package main

import (
	"errors"
	"os"
)

// The companion's production container runs Linux. Other development hosts report unavailable.
func diskSpace(string) (*storageSpace, error) { return nil, errors.New("storage_requires_linux") }
func sameDisk(os.FileInfo, os.FileInfo) bool  { return false }
