package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeConnector is a database/sql driver that answers every query with canned rows, so tests
// can reach row-level failures (scan errors, errors while iterating) that SQLite never produces.
type fakeConnector struct {
	cols    []string
	rows    [][]driver.Value
	nextErr error // returned by Next once the canned rows are consumed (io.EOF when nil)

	// Transaction mode: Begin, Prepare, Exec and Commit succeed unless the matching fault is set.
	txMode        bool
	failPrepareAt int // 1-based index of the Prepare call that fails (0: none); tx.Exec prepares too
	prepareCalls  int
	failCommit    bool
	failExecAt    int // 1-based index of the Exec call that fails (0: none)
	execCalls     int
	failBegin     bool // txMode only: Begin fails

	// failSQL fails the Prepare of any statement containing this text, so one chosen statement
	// of a multi-statement flow can fail (Exec and Query both prepare first).
	failSQL string
	// queryRows, when set, answers a Query per statement text; the column count follows the row width.
	queryRows func(query string) [][]driver.Value
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c}, nil }
func (c *fakeConnector) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

type fakeConn struct{ c *fakeConnector }

func (f *fakeConn) Prepare(query string) (driver.Stmt, error) {
	f.c.prepareCalls++
	if f.c.prepareCalls == f.c.failPrepareAt {
		return nil, errors.New("prepare boom")
	}
	if f.c.failSQL != "" && strings.Contains(query, f.c.failSQL) {
		return nil, errors.New("statement boom")
	}
	return &fakeStmt{f.c, query}, nil
}
func (f *fakeConn) Close() error { return nil }
func (f *fakeConn) Begin() (driver.Tx, error) {
	if f.c.txMode && !f.c.failBegin {
		return fakeTx{f.c}, nil
	}
	return nil, errors.New("begin boom")
}

type fakeTx struct{ c *fakeConnector }

func (t fakeTx) Commit() error {
	if t.c.failCommit {
		return errors.New("commit boom")
	}
	return nil
}
func (fakeTx) Rollback() error { return nil }

type fakeStmt struct {
	c     *fakeConnector
	query string
}

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	if s.c.txMode {
		s.c.execCalls++
		if s.c.execCalls == s.c.failExecAt {
			return nil, errors.New("exec boom")
		}
		return driver.RowsAffected(1), nil
	}
	return nil, errors.New("exec not supported")
}
func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	r := &fakeRows{c: s.c, cols: s.c.cols, rows: s.c.rows}
	if s.c.queryRows != nil {
		r.rows = s.c.queryRows(s.query)
		r.cols = make([]string, len(r.rows[0]))
	}
	return r, nil
}

type fakeRows struct {
	c    *fakeConnector
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.i < len(r.rows) {
		copy(dest, r.rows[r.i])
		r.i++
		return nil
	}
	if r.c.nextErr != nil {
		return r.c.nextErr
	}
	return io.EOF
}

func fakeStore(c *fakeConnector) *MessageStore {
	return &MessageStore{db: sql.OpenDB(c)}
}

func closedStore(t *testing.T) *MessageStore {
	t.Helper()
	s := newTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewMessageStoreErrors(t *testing.T) {
	t.Run("store dir under a file", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("WHATSAPP_STORE_DIR", filepath.Join(blocker, "store"))
		if _, err := NewMessageStore(); err == nil || !strings.Contains(err.Error(), "failed to create store directory") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("open fails", func(t *testing.T) {
		t.Setenv("WHATSAPP_STORE_DIR", t.TempDir())
		old := openMessagesDB
		openMessagesDB = func(string) (*sql.DB, error) { return nil, errors.New("driver gone") }
		t.Cleanup(func() { openMessagesDB = old })
		if _, err := NewMessageStore(); err == nil || !strings.Contains(err.Error(), "failed to open message database: driver gone") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("tables cannot be created", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("WHATSAPP_STORE_DIR", dir)
		// A directory where the database file must be
		if err := os.Mkdir(filepath.Join(dir, "messages.db"), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := NewMessageStore(); err == nil || !strings.Contains(err.Error(), "failed to create tables") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStoreQueriesFailOnClosedDB(t *testing.T) {
	s := closedStore(t)
	if _, err := s.GetMessages("a@s.whatsapp.net", 10); err == nil {
		t.Error("GetMessages: expected an error")
	}
	if _, err := s.GetChats(); err == nil {
		t.Error("GetChats: expected an error")
	}
	if _, err := s.chatsWithName(""); err == nil {
		t.Error("chatsWithName: expected an error")
	}
}

func TestStoreScanFailuresOnNullColumns(t *testing.T) {
	s := newTestStore(t)
	if err := s.StoreChat("a@s.whatsapp.net", "A", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO messages (id, chat_jid, timestamp) VALUES ('m', 'a@s.whatsapp.net', ?)", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMessages("a@s.whatsapp.net", 10); err == nil {
		t.Error("GetMessages must fail on a NULL sender")
	}
	if _, err := s.db.Exec("INSERT INTO chats (jid) VALUES ('b@s.whatsapp.net')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetChats(); err == nil {
		t.Error("GetChats must fail on a NULL last_message_time")
	}
}

func TestStoreRowIterationFailures(t *testing.T) {
	boom := errors.New("disk I/O error")
	t.Run("GetMessages", func(t *testing.T) {
		s := fakeStore(&fakeConnector{cols: []string{"a", "b", "c", "d", "e", "f"}, nextErr: boom})
		if _, err := s.GetMessages("x", 1); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("GetChats", func(t *testing.T) {
		s := fakeStore(&fakeConnector{cols: []string{"a", "b"}, nextErr: boom})
		if _, err := s.GetChats(); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chatsWithName iteration", func(t *testing.T) {
		s := fakeStore(&fakeConnector{cols: []string{"a", "b"}, nextErr: boom})
		if _, err := s.chatsWithName(""); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("chatsWithName scan", func(t *testing.T) {
		// One column where two are scanned
		s := fakeStore(&fakeConnector{cols: []string{"a"}, rows: [][]driver.Value{{"x"}}})
		if _, err := s.chatsWithName(""); err == nil || !strings.Contains(err.Error(), "destination arguments") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStoreHistoryBatchFailsClosed(t *testing.T) {
	msgs := []historyMessage{{id: "a", content: "x"}, {id: "b", content: "y"}}
	cases := []struct {
		name string
		c    *fakeConnector
		want string
	}{
		{"begin", &fakeConnector{}, "begin: "},
		{"chat upsert", &fakeConnector{txMode: true, failExecAt: 1}, "store chat: "},
		{"prepare", &fakeConnector{txMode: true, failPrepareAt: 2}, "prepare: "},
		{"message insert", &fakeConnector{txMode: true, failExecAt: 3}, "store message: "},
		{"commit", &fakeConnector{txMode: true, failCommit: true}, "commit: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := fakeStore(tc.c).storeHistoryBatch("a@s.whatsapp.net", "A", time.Now(), msgs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("success", func(t *testing.T) {
		c := &fakeConnector{txMode: true}
		if err := fakeStore(c).storeHistoryBatch("a@s.whatsapp.net", "A", time.Now(), msgs); err != nil {
			t.Fatal(err)
		}
		if c.execCalls != 3 {
			t.Fatalf("exec calls = %d, want chat upsert + 2 messages", c.execCalls)
		}
	})
}

func TestMigrateSearchFailsClosed(t *testing.T) {
	// counts answers the migration's count query: all three triggers present, then docsize and messages
	counts := func(indexed, total int64) func(string) [][]driver.Value {
		return func(string) [][]driver.Value { return [][]driver.Value{{int64(3), indexed, total}} }
	}
	cases := []struct {
		name string
		c    *fakeConnector
		want string
	}{
		{"index creation", &fakeConnector{txMode: true, failSQL: "idx_messages_chat_time"}, "create indexes: "},
		{"begin", &fakeConnector{txMode: true, failBegin: true}, "begin: "},
		{"fts table", &fakeConnector{txMode: true, failSQL: "CREATE VIRTUAL TABLE"}, "create messages_fts: "},
		{"triggers", &fakeConnector{txMode: true, failSQL: "CREATE TRIGGER"}, "create messages_fts triggers: "},
		{"count", &fakeConnector{txMode: true, failSQL: "messages_fts_docsize"}, "count indexed messages: "},
		// The docsize count differs from the messages count, so the rebuild runs
		{"rebuild", &fakeConnector{txMode: true, failSQL: "'rebuild'", queryRows: counts(1, 2)}, "rebuild messages_fts: "},
		{"commit", &fakeConnector{txMode: true, failCommit: true, queryRows: counts(2, 2)}, "commit: "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.queryRows == nil {
				tc.c.queryRows = counts(0, 0)
			}
			err := fakeStore(tc.c).migrateSearch(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("matching counts skip the rebuild", func(t *testing.T) {
		c := &fakeConnector{txMode: true, queryRows: counts(2, 2)}
		if err := fakeStore(c).migrateSearch(context.Background()); err != nil {
			t.Fatal(err)
		}
		// indexes, virtual table, triggers; no rebuild
		if c.execCalls != 3 {
			t.Fatalf("exec calls = %d, want 3", c.execCalls)
		}
	})
	t.Run("mismatched counts rebuild", func(t *testing.T) {
		c := &fakeConnector{txMode: true, queryRows: counts(0, 2)}
		if err := fakeStore(c).migrateSearch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c.execCalls != 4 {
			t.Fatalf("exec calls = %d, want 4", c.execCalls)
		}
	})
	t.Run("missing trigger rebuilds at matching counts", func(t *testing.T) {
		c := &fakeConnector{txMode: true, queryRows: func(string) [][]driver.Value {
			return [][]driver.Value{{int64(2), int64(2), int64(2)}}
		}}
		if err := fakeStore(c).migrateSearch(context.Background()); err != nil {
			t.Fatal(err)
		}
		if c.execCalls != 4 {
			t.Fatalf("exec calls = %d, want 4", c.execCalls)
		}
	})
}
