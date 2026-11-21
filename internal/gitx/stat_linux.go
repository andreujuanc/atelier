package gitx

import "syscall"

func statTimes(st *syscall.Stat_t) (mtime, ctime syscall.Timespec) { return st.Mtim, st.Ctim }
