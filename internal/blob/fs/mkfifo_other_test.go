//go:build !unix

package fs

import "errors"

func mkfifo(string) error { return errors.New("mkfifo not supported") }
