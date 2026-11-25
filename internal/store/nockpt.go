package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"unsafe"

	// modernc.org/libc must stay at the version modernc.org/sqlite requires
	// (go.mod): the driver is transpiled against it, and noCheckpointOnClose
	// calls into both. TestCloseSkipsCheckpoint and TestConnHandleGuard
	// check the pair after an upgrade.
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
	// (TestCloseSkipsCheckpoint notices).
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
// sqlite3* handle and libc TLS are read from its unexported fields, but
// only after connHandle has checked the connection's type and fields; on
// any mismatch nothing is read and the connection keeps
// checkpointing on close. conn must not be in use by another goroutine.
func noCheckpointOnClose(conn driver.Conn) error {
	db, tls, ok := connHandle(conn)
	if !ok {
		return errors.New("store: unexpected sqlite connection type")
	}
	// The C varargs (int onoff, int *pRes) of sqlite3_db_config, built as
	// modernc.org/sqlite v1.46.1 builds those of sqlite3_vtab_config
	// (vtabConfig, vtab.go:169): sqlite3_malloc'd, filled by libc.VaList
	// (8 bytes per argument), freed after the call.
	const vaSize = 16
	va := sqlite3.Xsqlite3_malloc(tls, vaSize)
	if va == 0 {
		return errors.New("store: out of memory")
	}
	defer sqlite3.Xsqlite3_free(tls, va)
	libc.VaList(va, int32(1), uintptr(0))
	if rc := sqlite3.Xsqlite3_db_config(tls, db, sqlite3.SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, va); rc != sqlite3.SQLITE_OK {
		return errors.New("store: sqlite3_db_config(SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE) failed")
	}
	return nil
}

// sqlitePkg is the import path of the modernc.org/sqlite driver, which
// declares its connection type, conn.
var sqlitePkg = reflect.TypeFor[*sqlite.Driver]().Elem().PkgPath()

// connHandle returns the sqlite3* handle and libc TLS of conn, a
// connection of the modernc.org/sqlite driver: its unexported fields
// db (uintptr) and tls (*libc.TLS). ok is false, and nothing is read, unless
// conn is a non-nil pointer to the driver's struct type conn with exactly
// those fields, and both are set.
func connHandle(conn any) (db uintptr, tls *libc.TLS, ok bool) {
	v := reflect.ValueOf(conn)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() {
		return 0, nil, false
	}
	e := v.Elem()
	if t := e.Type(); t.PkgPath() != sqlitePkg || t.Name() != "conn" || !connLayout(t) {
		return 0, nil, false
	}
	dbf, tlsf := e.FieldByName("db"), e.FieldByName("tls")
	if dbf.Uint() == 0 || tlsf.IsNil() {
		return 0, nil, false
	}
	tls = reflect.NewAt(tlsf.Type(), unsafe.Pointer(tlsf.UnsafeAddr())).Elem().Interface().(*libc.TLS)
	return uintptr(dbf.Uint()), tls, true
}

// connLayout reports whether struct type t has a field db of type uintptr
// and a field tls of type *libc.TLS (directly, not promoted from an
// embedded struct).
func connLayout(t reflect.Type) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	db, okDB := t.FieldByName("db")
	tls, okTLS := t.FieldByName("tls")
	return okDB && okTLS && len(db.Index) == 1 && len(tls.Index) == 1 &&
		db.Type == reflect.TypeFor[uintptr]() && tls.Type == reflect.TypeFor[*libc.TLS]()
}
