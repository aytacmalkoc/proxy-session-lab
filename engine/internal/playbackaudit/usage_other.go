//go:build !darwin

package playbackaudit

import "os"

func selfUsage() usage { return usage{} }
func childUsage(state *os.ProcessState) usage {
	if state == nil {
		return usage{}
	}
	return usage{user: state.UserTime().Seconds(), system: state.SystemTime().Seconds(), available: true}
}
