//go:build windows

package ufs

func ignoringEINTR(fn func() error) error {
	return fn()
}

func syscallMode(i FileMode) FileMode {
	return i.Perm()
}
