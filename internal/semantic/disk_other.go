//go:build !linux && !darwin && !windows

package semantic

func availableDisk(path string) (uint64, error) { return ^uint64(0), nil }
