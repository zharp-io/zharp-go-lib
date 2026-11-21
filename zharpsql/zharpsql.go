// Package zharpsql traces database/sql queries and reports the ones that fail, once, with the
// line of your code that ran them.
//
//	db, err := zharpsql.Open("pgx", os.Getenv("DATABASE_URL"))
//
// Each query becomes a CLIENT span in the request's trace (db.system, db.statement — the text with
// its placeholders, never the values). A query that fails is reported as an error whose stack is
// the call that ran it, with the statement, the database and the driver's code, named from the
// code (Postgres 23505 → UniqueViolation): as a crash when the error comes back up and the
// request is answered with a 5xx (zharpgin, zharpecho, or a 5xx from zharp.Handler), as handled
// when your code dealt with it. Never twice.
//
// It wraps any driver: lib/pq, pgx's stdlib, go-sql-driver/mysql, sqlite… and what's built on
// database/sql (GORM: postgres.New(postgres.Config{Conn: db}), sqlx.NewDb(db, "pgx"), bun).
// Queries outside a request (a job, a migration) make no span; a failure there is still reported.
package zharpsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const scope = "github.com/zharp-io/zharp-go-lib/zharpsql"

// Option configures Open, OpenDB and WrapDriver.
type Option func(*config)

// WithSystem names the database ("postgresql", "mysql", "sqlite", "mssql"); by default it is read
// from the driver's name.
func WithSystem(system string) Option { return func(c *config) { c.system = system } }

// WithDatabase names the database (db.name); by default it is read from the DSN when it can be.
func WithDatabase(name string) Option { return func(c *config) { c.database = name } }

// WithLibraries adds function prefixes ("github.com/acme/repo.") that sit between your code and
// database/sql, so the reported stack starts in your code. GORM, sqlx, bun, ent and squirrel are
// known already.
func WithLibraries(prefixes ...string) Option {
	return func(c *config) { c.libraries = append(c.libraries, prefixes...) }
}

type config struct {
	system, database string
	libraries        []string
}

var knownLibraries = []string{
	"gorm.io/", "github.com/jmoiron/sqlx.", "github.com/uptrace/bun", "entgo.io/", "github.com/Masterminds/squirrel.",
	"github.com/jackc/pgx/", "github.com/lib/pq.", "github.com/go-sql-driver/mysql.",
}

func newConfig(driverName, dsn string, opts []Option) *config {
	system := systemOf(driverName)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		system = "postgresql"
	} else if strings.HasPrefix(dsn, "mysql://") {
		system = "mysql"
	}
	c := &config{system: system, database: databaseOf(dsn), libraries: append([]string(nil), knownLibraries...)}
	for _, o := range opts {
		o(c)
	}
	return c
}

func systemOf(driverName string) string {
	switch n := strings.ToLower(driverName); {
	case strings.Contains(n, "postgres"), n == "pgx", n == "pq", strings.HasPrefix(n, "pgx"):
		return "postgresql"
	case strings.Contains(n, "mysql"), strings.Contains(n, "maria"):
		return "mysql"
	case strings.Contains(n, "sqlite"):
		return "sqlite"
	case strings.Contains(n, "sqlserver"), strings.Contains(n, "mssql"):
		return "mssql"
	case n == "":
		return "other_sql"
	default:
		return n
	}
}

// databaseOf reads the database's name from a URL DSN (postgres://…/shop) or key=value pairs
// (dbname=shop); "" when it can't.
func databaseOf(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		return strings.TrimPrefix(u.Path, "/")
	}
	for _, field := range strings.Fields(dsn) {
		if v, ok := strings.CutPrefix(field, "dbname="); ok {
			return strings.Trim(v, "'\"")
		}
	}
	return ""
}

// Open is sql.Open with every query traced and failures reported.
func Open(driverName, dsn string, opts ...Option) (*sql.DB, error) {
	probe, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, err
	}
	d := probe.Driver()
	_ = probe.Close()
	cfg := newConfig(driverName, dsn, opts)
	if dc, ok := d.(driver.DriverContext); ok {
		connector, err := dc.OpenConnector(dsn)
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(&tracedConnector{inner: connector, driver: d, cfg: cfg}), nil
	}
	return sql.OpenDB(&dsnConnector{dsn: dsn, driver: d, cfg: cfg}), nil
}

// OpenDB is sql.OpenDB with every query traced and failures reported — for drivers that hand
// out a Connector (pgx's stdlib.GetConnector, mysql.NewConnector).
func OpenDB(c driver.Connector, opts ...Option) *sql.DB {
	return sql.OpenDB(&tracedConnector{inner: c, driver: c.Driver(), cfg: newConfig(reflect.TypeOf(c.Driver()).String(), "", opts)})
}

// WrapDriver wraps a driver, for sql.Register("pgx-zharp", zharpsql.WrapDriver(stdlib.GetDefaultDriver())).
func WrapDriver(d driver.Driver, opts ...Option) driver.Driver {
	return &tracedDriver{inner: d, cfg: newConfig(reflect.TypeOf(d).String(), "", opts)}
}

type tracedDriver struct {
	inner driver.Driver
	cfg   *config
}

func (d *tracedDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &conn{inner: c, cfg: d.cfg}, nil
}

type tracedConnector struct {
	inner  driver.Connector
	driver driver.Driver
	cfg    *config
}

func (c *tracedConnector) Connect(ctx context.Context) (driver.Conn, error) {
	inner, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &conn{inner: inner, cfg: c.cfg}, nil
}

func (c *tracedConnector) Driver() driver.Driver { return &tracedDriver{inner: c.driver, cfg: c.cfg} }

type dsnConnector struct {
	dsn    string
	driver driver.Driver
	cfg    *config
}

func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) {
	inner, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &conn{inner: inner, cfg: c.cfg}, nil
}

func (c *dsnConnector) Driver() driver.Driver { return &tracedDriver{inner: c.driver, cfg: c.cfg} }

// ── the connection ──────────────────────────────────────────────────────────────

type conn struct {
	inner driver.Conn
	cfg   *config
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnPrepareContext = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
)

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	var s driver.Stmt
	err := c.cfg.do(ctx, query, func(context.Context) (err error) {
		if p, ok := c.inner.(driver.ConnPrepareContext); ok {
			s, err = p.PrepareContext(ctx, query)
		} else {
			s, err = c.inner.Prepare(query)
		}
		return err
	}, false)
	if err != nil {
		return nil, err
	}
	return &stmt{inner: s, query: query, cfg: c.cfg}, nil
}

func (c *conn) Close() error { return c.inner.Close() }

func (c *conn) Begin() (driver.Tx, error) { return c.BeginTx(context.Background(), driver.TxOptions{}) }

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.inner.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.inner.Begin() //nolint:staticcheck // the driver has nothing newer
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	e, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip // database/sql prepares instead, through PrepareContext
	}
	var result driver.Result
	err := c.cfg.do(ctx, query, func(ctx context.Context) (err error) {
		result, err = e.ExecContext(ctx, query, args)
		return err
	}, true)
	return result, err
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	var rows driver.Rows
	err := c.cfg.do(ctx, query, func(ctx context.Context) (err error) {
		rows, err = q.QueryContext(ctx, query, args)
		return err
	}, true)
	return rows, err
}

func (c *conn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *conn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *conn) IsValid() bool {
	if v, ok := c.inner.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := c.inner.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// ── prepared statements ─────────────────────────────────────────────────────────

type stmt struct {
	inner driver.Stmt
	query string
	cfg   *config
}

func (s *stmt) Close() error  { return s.inner.Close() }
func (s *stmt) NumInput() int { return s.inner.NumInput() }

func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	var result driver.Result
	err := s.cfg.do(ctx, s.query, func(ctx context.Context) (err error) {
		if e, ok := s.inner.(driver.StmtExecContext); ok {
			result, err = e.ExecContext(ctx, args)
		} else {
			result, err = s.inner.Exec(values(args)) //nolint:staticcheck
		}
		return err
	}, true)
	return result, err
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	var rows driver.Rows
	err := s.cfg.do(ctx, s.query, func(ctx context.Context) (err error) {
		if q, ok := s.inner.(driver.StmtQueryContext); ok {
			rows, err = q.QueryContext(ctx, args)
		} else {
			rows, err = s.inner.Query(values(args)) //nolint:staticcheck
		}
		return err
	}, true)
	return rows, err
}

func (s *stmt) CheckNamedValue(nv *driver.NamedValue) error {
	if n, ok := s.inner.(driver.NamedValueChecker); ok {
		return n.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

func (s *stmt) ColumnConverter(idx int) driver.ValueConverter {
	if c, ok := s.inner.(driver.ColumnConverter); ok { //nolint:staticcheck
		return c.ColumnConverter(idx)
	}
	return driver.DefaultParameterConverter
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

func values(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

// ── one query ───────────────────────────────────────────────────────────────────

// do runs one query in a span (inside a request) and hands a failure to Zharp. span is false for
// a Prepare: it is part of the query that follows, not a query of its own — but its failure (a
// syntax error, a missing table) is the query's failure.
func (c *config) do(ctx context.Context, query string, run func(context.Context) error, span bool) error {
	var s trace.Span
	if span && trace.SpanContextFromContext(ctx).IsValid() {
		attrs := []attribute.KeyValue{attribute.String("db.system", c.system), attribute.String("db.statement", query)}
		if op := operation(query); op != "" {
			attrs = append(attrs, attribute.String("db.operation", op))
		}
		if c.database != "" {
			attrs = append(attrs, attribute.String("db.name", c.database))
		}
		ctx, s = otel.Tracer(scope).Start(ctx, spanName(query, c.database), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
		defer s.End()
	}
	err := run(ctx)
	if err == nil || !reportable(err) {
		return err
	}
	code, constraint := details(err, c.system)
	if s != nil {
		// No exception event: the error is reported once, below.
		s.SetStatus(codes.Error, err.Error())
		if code != "" {
			s.SetAttributes(attribute.String("db.response.status_code", code))
		}
	}
	failure := zharp.QueryFailure{
		Err: err, System: c.system, Statement: query, Database: c.database, Code: code, Constraint: constraint,
		Stack: zharp.CallSite(1, c.libraries...),
	}
	if s != nil {
		failure.Span = s.SpanContext()
	}
	zharp.ReportFailedQuery(ctx, failure)
	return err
}

// reportable: errors that are the query's failure, not database/sql's own signals or the caller
// leaving.
func reportable(err error) bool {
	return !errors.Is(err, driver.ErrSkip) && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrNoRows) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, driver.ErrRemoveArgument)
}

// details reads the driver's code and constraint: SQLState() (pgx, lib/pq), Number (MySQL), and a
// ConstraintName/Constraint field when the error has one.
func details(err error, system string) (code, constraint string) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s, ok := e.(interface{ SQLState() string }); ok && code == "" {
			code = s.SQLState()
		}
		v := reflect.ValueOf(e)
		for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
			if v.IsNil() {
				break
			}
			v = v.Elem()
		}
		if v.Kind() == reflect.Struct {
			if constraint == "" {
				for _, name := range []string{"ConstraintName", "Constraint"} {
					if f := v.FieldByName(name); f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
						constraint = f.String()
						break
					}
				}
			}
			if code == "" && system == "mysql" {
				if f := v.FieldByName("Number"); f.IsValid() && f.CanUint() && f.Uint() != 0 {
					code = strconv.FormatUint(f.Uint(), 10)
				}
			}
		}
	}
	return code, constraint
}

// operation is a statement's first word, upper-cased: SELECT, INSERT…
func operation(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	op := strings.ToUpper(strings.Trim(fields[0], "("))
	for _, r := range op {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return op
}

func spanName(query, database string) string {
	op := operation(query)
	if op == "" {
		op = "query"
	}
	if database != "" {
		return op + " " + database
	}
	return op
}
