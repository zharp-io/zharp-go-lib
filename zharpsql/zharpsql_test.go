package zharpsql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

// ── a driver whose INSERTs fail like Postgres's unique violation ─────────────────

type pgError struct{ Code, ConstraintName string }

func (e *pgError) Error() string {
	return `ERROR: duplicate key value violates unique constraint "` + e.ConstraintName + `" (SQLSTATE ` + e.Code + `)`
}
func (e *pgError) SQLState() string { return e.Code }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, fmt.Errorf("not used") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, fmt.Errorf("not used") }
func (fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if strings.HasPrefix(query, "INSERT") {
		return nil, &pgError{Code: "23505", ConstraintName: "users_email_key"}
	}
	return driver.RowsAffected(1), nil
}

func init() { sql.Register("fakepg", fakeDriver{}) }

// ── setup ───────────────────────────────────────────────────────────────────────

type memoryLogs struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memoryLogs) Export(_ context.Context, records []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memoryLogs) Shutdown(context.Context) error   { return nil }
func (m *memoryLogs) ForceFlush(context.Context) error { return nil }

func (m *memoryLogs) errors() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []map[string]string
	for _, r := range m.records {
		a := map[string]string{}
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			a[string(kv.Key)] = kv.Value.Emit()
			return true
		})
		if a["exception.type"] != "" {
			out = append(out, a)
		}
	}
	return out
}

func setup(t *testing.T) (*sql.DB, *tracetest.InMemoryExporter, *memoryLogs) {
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "sql-unit", SpanExporter: spans, LogExporter: logs})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	db, err := Open("fakepg", "postgres://u@localhost/shop")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, spans, logs
}

func createUser(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "INSERT INTO users (email) VALUES ($1)", "ada@example.com")
	return err
}

func request(handler http.HandlerFunc) {
	w := httptest.NewRecorder()
	zharp.Handler(handler).ServeHTTP(w, httptest.NewRequest("POST", "/signup", nil))
}

// ── tests ───────────────────────────────────────────────────────────────────────

func TestCaughtFailureReportedOnceAsHandledWithCallSite(t *testing.T) {
	db, spans, logs := setup(t)
	request(func(w http.ResponseWriter, r *http.Request) {
		if err := createUser(r.Context(), db); err != nil {
			w.WriteHeader(http.StatusConflict) // the app dealt with it
		}
	})
	errs := logs.errors()
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %d: %v", len(errs), errs)
	}
	e := errs[0]
	if e["exception.type"] != "UniqueViolation" || e["zharp.handled"] != "true" || e["zharp.mechanism"] != "db" {
		t.Errorf("type/handled/mechanism: %v", e)
	}
	if e["db.statement"] != "INSERT INTO users (email) VALUES ($1)" || e["db.error.code"] != "23505" ||
		e["db.error.constraint"] != "users_email_key" || e["db.name"] != "shop" || e["db.system"] != "postgresql" {
		t.Errorf("query attributes: %v", e)
	}
	stack := e["exception.stacktrace"]
	if !strings.Contains(stack, "zharpsql.createUser") || strings.Contains(stack, "database/sql.") || strings.Contains(stack, "zharpsql.(*config).do") {
		t.Errorf("stack should start at the app's call:\n%s", stack)
	}
	var query *tracetest.SpanStub
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindClient {
			query = &s
		}
	}
	if query == nil || query.Name != "INSERT shop" || len(query.Events) != 0 || query.Status.Code.String() != "Error" {
		t.Fatalf("query span: %+v", query)
	}
}

func TestFailureInA5xxRequestIsACrash(t *testing.T) {
	db, _, logs := setup(t)
	request(func(w http.ResponseWriter, r *http.Request) {
		if err := createUser(r.Context(), db); err != nil {
			http.Error(w, "internal", http.StatusInternalServerError)
		}
	})
	errs := logs.errors()
	if len(errs) != 1 || errs[0]["zharp.handled"] != "false" {
		t.Fatalf("want one unhandled error: %v", errs)
	}
}

func TestReportedErrorClaimsItsQuery(t *testing.T) {
	db, _, logs := setup(t)
	request(func(w http.ResponseWriter, r *http.Request) {
		if err := createUser(r.Context(), db); err != nil {
			zharp.CaptureError(r.Context(), fmt.Errorf("signup: %w", err))
		}
	})
	errs := logs.errors()
	if len(errs) != 1 {
		t.Fatalf("want 1 error, got %d", len(errs))
	}
	if !strings.HasPrefix(errs[0]["exception.message"], "signup: ERROR: duplicate key") || errs[0]["db.statement"] == "" {
		t.Errorf("the app's wrapping, with the query: %v", errs[0])
	}
}

func TestOutsideARequestStillReported(t *testing.T) {
	db, spans, logs := setup(t)
	if err := createUser(context.Background(), db); err == nil {
		t.Fatal("want an error")
	}
	_ = zharp.Flush(context.Background())
	if errs := logs.errors(); len(errs) != 1 {
		t.Fatalf("want 1 error, got %d", len(errs))
	}
	if len(spans.GetSpans()) != 0 {
		t.Errorf("no span outside a request")
	}
}

func TestSystemAndDatabaseFromTheDSN(t *testing.T) {
	cases := map[string][2]string{"pgx": {"postgresql", ""}, "postgres": {"postgresql", ""}, "mysql": {"mysql", ""}, "sqlite3": {"sqlite", ""}}
	for name, want := range cases {
		if got := systemOf(name); got != want[0] {
			t.Errorf("%s: %s", name, got)
		}
	}
	if databaseOf("postgres://u:p@h:5432/shop?sslmode=disable") != "shop" || databaseOf("host=h dbname=shop user=u") != "shop" {
		t.Error("database name")
	}
}
