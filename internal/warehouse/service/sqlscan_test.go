package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/warehouse"
)

func TestGuard(t *testing.T) {
	type tc struct {
		sql  string
		want error // nil = allowed
	}
	cases := map[string][]tc{
		"postgres": {
			{"SELECT 1", nil},
			{"  select 1", nil},
			{"\n\tSeLeCt 1", nil},
			{"-- comment\nSELECT 1", nil},
			{"/* c */ SELECT 1", nil},
			{"/* multi\nline */\n\t-- x\n  WITH t AS (SELECT 1) SELECT * FROM t", nil},
			{"WITH RECURSIVE r(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM r WHERE n < 3) SELECT * FROM r", nil},
			{"(SELECT 1) UNION (SELECT 2)", nil},
			{"( /* x */ ( SELECT 1 ) )", nil},
			{"SELECT/*c*/1", nil},
			{"SELECT 1;", nil},
			{"SELECT 1;  -- trailing\n", nil},
			{"SELECT 1; ; /* x */ ;", nil},
			{"SELECT ';' AS semi", nil},
			{`SELECT 'a;b', "we;ird" FROM t`, nil},
			{"SELECT 'it''s; fine'", nil},
			{`SELECT E'it\'s; fine'`, nil},
			{`SELECT e'\\'`, nil},
			{"SELECT col FROM t WHERE x = '--' AND y = 1", nil},
			{"SELECT 1 -- ; DROP TABLE x", nil},
			{"SELECT 1 /* ; DROP TABLE x */", nil},
			{"SELECT $1::int", nil},
			{"SELECT a$b FROM t", nil},
			// The guard follows SPEC: only the leading keyword is checked;
			// the READ ONLY transaction stops data-modifying CTEs.
			{"WITH x AS (DELETE FROM t RETURNING *) SELECT * FROM x", nil},

			{"DELETE FROM t", warehouse.ErrForbidden},
			{"INSERT INTO t VALUES (1)", warehouse.ErrForbidden},
			{"  drop table t", warehouse.ErrForbidden},
			{"selectx 1", warehouse.ErrForbidden},
			{"SELECTED", warehouse.ErrForbidden},
			{"WITHDRAW", warehouse.ErrForbidden},
			{"-- SELECT\nDELETE FROM t", warehouse.ErrForbidden},
			{"/* SELECT */ UPDATE t SET a = 1", warehouse.ErrForbidden},
			{"(DELETE FROM t)", warehouse.ErrForbidden},
			{"'SELECT' 1", warehouse.ErrForbidden},
			{`"select" FROM t`, warehouse.ErrForbidden},
			{"; SELECT 1", warehouse.ErrForbidden},
			{"SELECT 1; DELETE FROM t", warehouse.ErrForbidden},
			{"SELECT 1; SELECT 2", warehouse.ErrForbidden},
			{"SELECT 1;\nDELETE FROM t;", warehouse.ErrForbidden},
			{"SELECT 1;-- x\nDELETE FROM t", warehouse.ErrForbidden},
			{`SELECT 'x\'; DELETE FROM t; --'`, warehouse.ErrForbidden},
			// Standard strings hide the ';' but a backslash-escaping engine
			// (Redshift) would run two statements.
			{`SELECT '\' , '; DELETE FROM t; --'`, warehouse.ErrForbidden},
			// Engines disagree on nested comments.
			{"/* /* */ SELECT 1 */ DELETE FROM t", warehouse.ErrForbidden},
			{"SELECT 1 /* /* */", warehouse.ErrBadRequest}, // unterminated when nested
			// Dollar quotes are unknown to legacy engines: conservative.
			{"SELECT $$a;b$$", warehouse.ErrForbidden},

			{"SELECT 1 /* unterminated", warehouse.ErrBadRequest},
			{"SELECT 'unterminated", warehouse.ErrBadRequest},
			{`SELECT "unterminated`, warehouse.ErrBadRequest},
			{"SELECT $tag$ unterminated", warehouse.ErrBadRequest},

			// Line comments end at CR as well as LF.
			{"SELECT 1 -- c\r\n", nil},
			{"-- c\r\nSELECT 1", nil},
			{"-- c\rSELECT 1", nil},
			{"SELECT 1 --\r; DELETE FROM t\n", warehouse.ErrForbidden},
			{"--\rDELETE FROM t; --\nSELECT 1", warehouse.ErrForbidden},
			// Accepted by standard + legacy readings, but PostgreSQL with
			// standard_conforming_strings=off (backslash escapes AND dollar
			// quotes) sees a ';' in code.
			{"SELECT $q$ /* $q$ 'a\\' ' ; */ ' -- '\n", warehouse.ErrForbidden},
			// … and engines without dollar quotes or backslash escapes.
			{"SELECT $q$ ' $q$\\' ; ' -- '\n", warehouse.ErrForbidden},
			// PostgreSQL ≤ 14 lexes 1E'…' as 1 followed by an escape string.
			{`SELECT 1E'\' ' ; DELETE FROM t -- '`, warehouse.ErrForbidden},
			{`SELECT 1e'x'`, warehouse.ErrForbidden},
			{`SELECT x.e'it\'s;'`, nil},
		},
		"bigquery": {
			{"# comment\nSELECT 1", nil},
			{`SELECT "a;b"`, nil},
			{`SELECT "it\"s;"`, nil},
			{`SELECT 'it\'s;'`, nil},
			{"SELECT `weird;name` FROM t", nil},
			{"SELECT '''a;\n'b'''", nil},
			{`SELECT """a;"b"""`, nil},
			{`SELECT r'\'; x'`, nil},
			{"SELECT 1 # ; DROP", nil},
			{"SELECT @@project_id", nil},

			{"SELECT 1; DELETE FROM t", warehouse.ErrForbidden},
			{"DECLARE x INT64; SELECT x", warehouse.ErrForbidden},
			{"# SELECT\nDELETE FROM t WHERE true", warehouse.ErrForbidden},
			{"EXPORT DATA OPTIONS(uri='gs://x/*') AS SELECT 1", warehouse.ErrForbidden},
			{"SELECT '''unterminated", warehouse.ErrBadRequest},

			// GoogleSQL scripts run several statements in one job, so a
			// comment ending at a bare CR must not hide one.
			{"--\rDELETE FROM t WHERE true; --\nSELECT 1", warehouse.ErrForbidden},
			{"SELECT 1 #\r; DELETE FROM t WHERE true", warehouse.ErrForbidden},
			{"#\rDELETE FROM t WHERE true; #\nSELECT 1", warehouse.ErrForbidden},
			{"SELECT 1 # c\r\n", nil},
			// GoogleSQL has no doubled-quote escape: '' ends a literal.
			{"SELECT 'a'''' C ' D ''' ; DELETE FROM t WHERE true ; ''' F 'x''' AS z", warehouse.ErrForbidden},
			{"SELECT 'a' 'b;'", nil},
			{"SELECT `a``b;` FROM t", nil},
		},
		"athena": {
			{`SELECT "a;b" FROM t`, nil},
			{"SELECT 'it''s;'", nil},
			{"SELECT 1 -- x", nil},

			{`SELECT 'a\'; DELETE FROM t; --'`, warehouse.ErrForbidden},
			{"# x\nSELECT 1", warehouse.ErrForbidden},
			{"SELECT `a;b`", warehouse.ErrForbidden},
			{"CREATE TABLE x AS SELECT 1", warehouse.ErrForbidden},
			{"MSCK REPAIR TABLE t", warehouse.ErrForbidden},
			{"SELECT 1 --\r; DELETE FROM t\n", warehouse.ErrForbidden},
			{"SELECT 'a''; DELETE FROM t; '", nil},
		},
	}
	for typ, list := range cases {
		for _, c := range list {
			t.Run(typ+"/"+c.sql, func(t *testing.T) {
				err := guard(c.sql, dialectsFor(typ))
				switch {
				case c.want == nil && err != nil:
					t.Fatalf("guard(%q) = %v, want allowed", c.sql, err)
				case c.want != nil && !errors.Is(err, c.want):
					t.Fatalf("guard(%q) = %v, want %v", c.sql, err, c.want)
				}
			})
		}
	}
}

// TestGuardNeedsEveryReading documents why each lexical reading exists: every
// statement below passes a guard that lacks one reading (or rule), yet some
// engine of that source type sees a statement separator in it.
func TestGuardNeedsEveryReading(t *testing.T) {
	bqDoubling := dialectBigQuery
	bqDoubling.noQuoteDoubling = false
	cases := []struct {
		name, typ, sql string
		weaker         []dialect
	}{
		{"pg backslash escapes with dollar quotes", "postgres",
			"SELECT $q$ /* $q$ 'a\\' ' ; */ ' -- '\n",
			[]dialect{dialectPostgres, dialectPostgresLegacy, dialectPostgresPlain}},
		{"pg standard strings without dollar quotes", "postgres",
			"SELECT $q$ ' $q$\\' ; ' -- '\n",
			[]dialect{dialectPostgres, dialectPostgresEscaping, dialectPostgresLegacy}},
		{"googlesql has no doubled quotes", "bigquery",
			"SELECT 'a'''' C ' D ''' ; DELETE FROM t WHERE true ; ''' F 'x''' AS z",
			[]dialect{bqDoubling}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := guard(c.sql, c.weaker); err != nil {
				t.Fatalf("weaker guard rejects %q: %v (the case no longer documents anything)", c.sql, err)
			}
			if err := guard(c.sql, dialectsFor(c.typ)); !errors.Is(err, warehouse.ErrForbidden) {
				t.Fatalf("guard(%q) = %v, want ErrForbidden", c.sql, err)
			}
		})
	}
}

func TestScanContentEnd(t *testing.T) {
	cases := []struct{ sql, want string }{
		{"SELECT 1", "SELECT 1"},
		{"SELECT 1;", "SELECT 1"},
		{"SELECT 1 ; ;\n-- c\n/* d */  ", "SELECT 1"},
		{"SELECT ';'", "SELECT ';'"},
		{"SELECT 1 -- c", "SELECT 1"},
		{"SELECT 1 -- c\r", "SELECT 1"},
		{"SELECT 1 -- c\r2", "SELECT 1 -- c\r2"},
		{"  -- only\n", ""},
		{";;", ""},
		{`SELECT "x"`, `SELECT "x"`},
	}
	for _, c := range cases {
		sc, err := scanSQL(c.sql, dialectPostgres)
		if err != nil {
			t.Fatalf("scan(%q): %v", c.sql, err)
		}
		if got := c.sql[:sc.contentEnd()]; got != c.want {
			t.Errorf("content of %q = %q, want %q", c.sql, got, c.want)
		}
	}
}

func TestTranslateQuestionMarks(t *testing.T) {
	pg := func(i int) string { return "$" + string(rune('1'+i)) }
	q := func(int) string { return "?" }
	cases := []struct {
		name    string
		d       dialect
		ph      func(int) string
		sql     string
		want    string
		nParams int
	}{
		{"pg basic", dialectPostgres, pg, "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2", 2},
		{"pg literals", dialectPostgres, pg,
			"SELECT * FROM t WHERE a = ? AND b = '?' AND c = \"?\" -- ?\n AND d = ? /* ? */",
			"SELECT * FROM t WHERE a = $1 AND b = '?' AND c = \"?\" -- ?\n AND d = $2", 2},
		{"pg dollar", dialectPostgres, pg, "SELECT $$?$$, $q$ ? $q$, ?", "SELECT $$?$$, $q$ ? $q$, $1", 1},
		{"pg estring", dialectPostgres, pg, `SELECT E'\'?', ?`, `SELECT E'\'?', $1`, 1},
		{"pg nested comment", dialectPostgres, pg, "SELECT /* a /* ? */ ? */ ?", "SELECT /* a /* ? */ ? */ $1", 1},
		{"pg trailing semicolon", dialectPostgres, pg, "SELECT ?;", "SELECT $1", 1},
		{"pg comment ends at CR", dialectPostgres, pg, "SELECT ? -- ?\r, ?", "SELECT $1 -- ?\r, $2", 2},
		{"pg none", dialectPostgres, pg, "SELECT 1", "SELECT 1", 0},
		{"bq", dialectBigQuery, q, "SELECT ?, '?', \"?\", `?` # ?\n, ? -- end", "SELECT ?, '?', \"?\", `?` # ?\n, ?", 2},
		{"bq escaped quote", dialectBigQuery, q, `SELECT 'a\'?', ?`, `SELECT 'a\'?', ?`, 1},
		{"bq no doubled quotes", dialectBigQuery, q, `SELECT 'a''?'`, `SELECT 'a''?'`, 0},
		{"bq hash comment ends at CR", dialectBigQuery, q, "SELECT ? # ?\r, ?", "SELECT ? # ?\r, ?", 2},
		{"athena", dialectTrino, q, `SELECT 'it''s ?' , "?", ?`, `SELECT 'it''s ?' , "?", ?`, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc, err := scanSQL(c.sql, c.d)
			if err != nil {
				t.Fatal(err)
			}
			end := sc.contentEnd()
			slots := sc.questionSlots(end)
			if len(slots) != c.nParams {
				t.Fatalf("slots = %d, want %d", len(slots), c.nParams)
			}
			if got := newTemplate(c.sql, end, slots).render(c.ph); got != c.want {
				t.Fatalf("render = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAtSlots(t *testing.T) {
	cases := []struct {
		d    dialect
		sql  string
		want []string
	}{
		{dialectPostgres, "SELECT @a, @b_2, @a", []string{"a", "b_2", "a"}},
		{dialectPostgres, "SELECT '@a', \"@b\", @c -- @d\n /* @e */", []string{"c"}},
		{dialectPostgres, "SELECT x@y, @@z, a @> b, @ -5, @_ok", []string{"_ok"}},
		{dialectBigQuery, "SELECT @@dataset_id, @team, `@x`, \"@y\" # @z", []string{"team"}},
		{dialectPostgres, "SELECT @1, @é", nil},
	}
	for _, c := range cases {
		sc, err := scanSQL(c.sql, c.d)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, s := range sc.atSlots(len(c.sql)) {
			got = append(got, s.name)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("atSlots(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func FuzzGuard(f *testing.F) {
	for _, s := range []string{"SELECT 1", "SELECT ';'; DELETE", "/* /* */", `E'\'`, "$$;$$", "'''", "`x`", "-- x\n", "SELECT ?", "-- x\rSELECT 1", "SELECT 1e'x'", "SELECT 'a''b'"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		for _, typ := range []string{"postgres", "bigquery", "athena"} {
			ds := dialectsFor(typ)
			err := guard(sql, ds)
			if err == nil {
				// Anything accepted must start with SELECT/WITH and have no
				// separator in its content under every reading.
				for _, d := range ds {
					sc, serr := scanSQL(sql, d)
					if serr != nil {
						t.Fatalf("accepted unscannable %q", sql)
					}
					if kw := sc.leadingKeyword(); kw != "SELECT" && kw != "WITH" {
						t.Fatalf("accepted %q with keyword %q", sql, kw)
					}
					if sc.hasStatementSeparator(sc.contentEnd()) {
						t.Fatalf("accepted multi-statement %q", sql)
					}
				}
			}
			if sc, err := scanSQL(sql, ds[0]); err == nil {
				end := sc.contentEnd()
				_ = newTemplate(sql, end, sc.questionSlots(end)).render(func(int) string { return "?" })
				_ = sc.atSlots(end)
			}
		}
	})
}
