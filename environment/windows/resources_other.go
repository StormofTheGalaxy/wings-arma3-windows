//go:build !windows

package windows

import (
	"time"

	"github.com/pterodactyl/wings/environment"
)

type networkCounters struct {
	rx uint64
	tx uint64
}

func (e *Environment) resourceStats(uint64, time.Time, networkCounters) (environment.Stats, uint64, time.Time) {
	return environment.Stats{}, 0, time.Now()
}

func currentNetworkCounters() (networkCounters, error) {
	return networkCounters{}, nil
}
