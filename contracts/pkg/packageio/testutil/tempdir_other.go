//go:build !windows

package testutil

func secureDirectory(string) error { return nil }
