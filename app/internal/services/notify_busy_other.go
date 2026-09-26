//go:build !windows

package services

// userBusy — определение полноэкранного режима есть только на Windows.
func userBusy() bool { return false }
