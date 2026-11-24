package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"unsafe"

	"modernc.org/libc"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// noCkptConnector opens read-write connections with the sqlite driver and
// turns off each one's checkpoint on close (see noCheckpointOnClose).
type noCkptConnector struct {
	dsn string
	drv *sqlite.Driver
}

func (c noCkptConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.drv.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	// Best effort: should the driver's internals change, the connection
	// still works and merely checkpoints on close as before
	// (TestNoCheckpointOnClose notices).
	_ = noCheckpointOnClose(conn)
	return conn, nil
}

func (c noCkptConnector) Driver() driver.Driver { return c.drv }

// noCheckpointOnClose sets SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE on conn, a
// connection of the modernc.org/sqlite driver.
//
// By default the last connection to a WAL database to close checkpoints
// the WAL into the database file, fsyncs both and deletes the -wal and
// -shm files. Every `atl hook` (one per Claude tool call) is that last
// connection whenever the TUI is not running, which makes it several
// times slower than with the TUI open. Committed transactions are in the
// WAL either way (as durable as synchronous=NORMAL makes them, the same as
// while the TUI holds the database open); Store.Close checkpoints the WAL
// once it has grown, which keeps it bounded.
//
// The driver does not expose sqlite3_db_config, so the connection's
// sqlite3* handle and libc TLS are read from its unexported fields. conn
// must not be in use by another goroutine.
func noCheckpointOnClose(conn driver.Conn) error {
	v := reflect.ValueOf(conn)
	if v.Kind() != reflect.Pointer || v.Elem().Kind() != reflect.Struct {
		return errors.New("store: unexpected sqlite connection type")
	}
	db, tlsf := v.Elem().FieldByName("db"), v.Elem().FieldByName("tls")
	if db.Kind() != reflect.Uintptr || db.Uint() == 0 || !tlsf.IsValid() || tlsf.Type() != reflect.TypeFor[*libc.TLS]() || tlsf.IsNil() {
		return errors.New("store: unexpected sqlite connection layout")
	}
	tls := reflect.NewAt(tlsf.Type(), unsafe.Pointer(tlsf.UnsafeAddr())).Elem().Interface().(*libc.TLS)
	// The C varargs (int onoff, int *pRes) of sqlite3_db_config.
	const vaSize = 16
	va := tls.Alloc(vaSize)
	defer tls.Free(vaSize)
	libc.VaList(va, int32(1), uintptr(0))
	if rc := sqlite3.Xsqlite3_db_config(tls, uintptr(db.Uint()), sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, va); rc != sqlite3.SQLITE_OK {
		return errors.New("store: sqlite3_db_config(SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE) failed")
	}
	return nil
}
