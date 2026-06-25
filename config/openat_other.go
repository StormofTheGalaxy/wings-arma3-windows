//go:build !linux

package config

func openat2Supported() bool { return false }
