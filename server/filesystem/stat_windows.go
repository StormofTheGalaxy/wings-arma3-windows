//go:build windows

package filesystem

import "time"

func (s *Stat) CTime() time.Time {
	return s.ModTime()
}
