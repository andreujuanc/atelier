package gitx

import "syscall"

func statTimes(st *syscall.Stat_t) (mtime, ctime syscall.Timespec) { return st.Mtimespec, st.Ctimespec }
