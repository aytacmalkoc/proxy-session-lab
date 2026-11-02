package playbackaudit

import (
	"os"
	"syscall"
)

func selfUsage() usage {
	var r syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &r) != nil {
		return usage{}
	}
	return usage{user: float64(r.Utime.Sec) + float64(r.Utime.Usec)/1e6, system: float64(r.Stime.Sec) + float64(r.Stime.Usec)/1e6, rss: &r.Maxrss, available: true}
}
func childUsage(state *os.ProcessState) usage {
	if state == nil {
		return usage{}
	}
	u := usage{user: state.UserTime().Seconds(), system: state.SystemTime().Seconds(), available: true}
	if r, ok := state.SysUsage().(*syscall.Rusage); ok {
		u.rss = &r.Maxrss
	}
	return u
}
