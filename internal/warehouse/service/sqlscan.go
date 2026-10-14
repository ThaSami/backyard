package service

import (
	"errors"
	"strings"
)

// dialect describes the lexical rules of an engine's SQL that matter for
// finding placeholders, statement separators and the leading keyword: which
// constructs are string literals, quoted identifiers or comments (inside
// which "?", "@name" and ";" mean nothing).
//
// Line comments ("--", and "#" where enabled) end at a carriage return or a
// line feed: PostgreSQL, Trino and GoogleSQL all treat a lone "\r" as a line
// end, so a scanner that only stopped at "\n" would hide code from the guard.
type dialect struct {
	name              string
	backslashEscapes  bool // "\" escapes the next character in '…' (and "…" when doubleQuoteString) and `…`
	escapeStrings     bool // E'…' literals with backslash escapes (Postgres)
	doubleQuoteString bool // "…" is a string literal (BigQuery); otherwise a quoted identifier
	backtickIdent     bool // `…` is a quoted identifier (BigQuery)
	tripleQuotes      bool // '''…''' and """…""" literals (BigQuery)
	hashComments      bool // "#" starts a line comment (BigQuery)
	dollarQuotes      bool // $$…$$ / $tag$…$tag$ literals (Postgres)
	nestedComments    bool // "/*" nests inside block comments (Postgres)
	// noQuoteDoubling: a doubled quote character ends the literal (and the
	// next one opens another) instead of standing for one quote. GoogleSQL
	// has no doubled-quote escape; SQL-standard engines do.
	noQuoteDoubling bool
}

var (
	// dialectPostgres: PostgreSQL ≥ 9.1 (standard_conforming_strings on).
	dialectPostgres = dialect{name: "postgres", escapeStrings: true, dollarQuotes: true, nestedComments: true}
	// dialectPostgresEscaping: PostgreSQL with standard_conforming_strings
	// off, where a backslash escapes in every string literal.
	dialectPostgresEscaping = dialect{name: "postgres (backslash escapes)", backslashEscapes: true, escapeStrings: true, dollarQuotes: true, nestedComments: true}
	// dialectPostgresLegacy models Postgres-compatible engines that treat a
	// backslash as an escape in every string and know neither dollar quotes
	// nor nested comments (Amazon Redshift).
	dialectPostgresLegacy = dialect{name: "postgres (legacy strings)", backslashEscapes: true, escapeStrings: true}
	// dialectPostgresPlain models Postgres-compatible engines with standard
	// strings but without dollar quotes (older CockroachDB and others).
	dialectPostgresPlain = dialect{name: "postgres (no dollar quotes)", escapeStrings: true}
	// dialectBigQuery: GoogleSQL.
	dialectBigQuery = dialect{name: "bigquery", backslashEscapes: true, doubleQuoteString: true, backtickIdent: true, tripleQuotes: true, hashComments: true, noQuoteDoubling: true}
	// dialectTrino: Athena (Trino / Presto).
	dialectTrino = dialect{name: "athena"}
)

// dialectsFor returns the lexical readings of a source type; the first one is
// used to translate placeholders, all of them must pass the statement guard.
// A "postgres" source may be any Postgres-compatible engine, so the guard
// requires a statement to be safe under every combination of backslash
// escapes and dollar quotes such engines implement.
func dialectsFor(sourceType string) []dialect {
	switch sourceType {
	case "postgres":
		return []dialect{dialectPostgres, dialectPostgresEscaping, dialectPostgresLegacy, dialectPostgresPlain}
	case "bigquery":
		return []dialect{dialectBigQuery}
	default:
		return []dialect{dialectTrino}
	}
}

type spanKind uint8

const (
	spanString  spanKind = iota + 1 // string literal, including quotes and E prefix
	spanIdent                       // quoted identifier
	spanComment                     // line or block comment
)

// span is a non-code region of the SQL text: [start, end).
type span struct {
	kind       spanKind
	start, end int
}

// scanned is a lexed statement: the non-code spans in order. Everything
// between spans is plain code (keywords, identifiers, numbers, operators,
// whitespace, punctuation).
type scanned struct {
	sql   string
	spans []span
	// nestedOpen is set when "/*" occurs inside a block comment. Engines
	// disagree on whether block comments nest, so the guard rejects these.
	nestedOpen bool
	// ambiguousPrefix is set when a quote follows "<digit>E" (escapeStrings
	// dialects only): PostgreSQL ≤ 14 lexes "1E'…'" as the number 1 followed
	// by an escape string, newer versions reject it, and "x1E'…'" is an
	// identifier followed by a plain string. The guard rejects these.
	ambiguousPrefix bool
}

var (
	errUnterminatedString  = errors.New("unterminated quoted string")
	errUnterminatedIdent   = errors.New("unterminated quoted identifier")
	errUnterminatedComment = errors.New("unterminated block comment")
)

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || isDigit(c) || c == '$'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// scanSQL lexes sql under d.
func scanSQL(sql string, d dialect) (*scanned, error) {
	s := &scanned{sql: sql}
	n := len(sql)
	i := 0
	for i < n {
		c := sql[i]
		var next byte
		if i+1 < n {
			next = sql[i+1]
		}
		switch {
		case c == '-' && next == '-', c == '#' && d.hashComments:
			end := strings.IndexAny(sql[i:], "\r\n")
			if end < 0 {
				end = n
			} else {
				end += i
			}
			s.spans = append(s.spans, span{spanComment, i, end})
			i = end

		case c == '/' && next == '*':
			end, err := s.blockComment(i, d)
			if err != nil {
				return nil, err
			}
			s.spans = append(s.spans, span{spanComment, i, end})
			i = end

		case c == '\'' || c == '"' && d.doubleQuoteString:
			start := i
			escapes := d.backslashEscapes
			if d.escapeStrings && c == '\'' && i > 0 && (sql[i-1] == 'e' || sql[i-1] == 'E') {
				switch {
				case i < 2 || !isIdentChar(sql[i-2]):
					escapes = true
					start = i - 1
				case isDigit(sql[i-2]):
					s.ambiguousPrefix = true
				}
			}
			end, err := quoted(sql, i, c, escapes, d.tripleQuotes, !d.noQuoteDoubling)
			if err != nil {
				return nil, errUnterminatedString
			}
			s.spans = append(s.spans, span{spanString, start, end})
			i = end

		case c == '"', c == '`' && d.backtickIdent:
			end, err := quoted(sql, i, c, c == '`' && d.backslashEscapes, false, !d.noQuoteDoubling)
			if err != nil {
				return nil, errUnterminatedIdent
			}
			s.spans = append(s.spans, span{spanIdent, i, end})
			i = end

		case c == '$' && d.dollarQuotes && (i == 0 || !isIdentChar(sql[i-1])):
			tag := dollarTag(sql, i)
			if tag == "" {
				i++
				continue
			}
			body := strings.Index(sql[i+len(tag):], tag)
			if body < 0 {
				return nil, errUnterminatedString
			}
			end := i + len(tag) + body + len(tag)
			s.spans = append(s.spans, span{spanString, i, end})
			i = end

		default:
			i++
		}
	}
	return s, nil
}

// blockComment returns the end of the block comment starting at i.
func (s *scanned) blockComment(i int, d dialect) (int, error) {
	sql := s.sql
	depth := 1
	j := i + 2
	for j < len(sql) {
		switch {
		case sql[j] == '/' && j+1 < len(sql) && sql[j+1] == '*':
			s.nestedOpen = true
			if d.nestedComments {
				depth++
			}
			j += 2
		case sql[j] == '*' && j+1 < len(sql) && sql[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j, nil
			}
		default:
			j++
		}
	}
	return 0, errUnterminatedComment
}

// quoted returns the end (exclusive) of the quoted construct opening with q
// at i. With doubling, a doubled quote character continues the construct;
// with escapes a backslash consumes the following byte; with triple, a run of
// three quotes opens a triple-quoted literal that only three quotes close.
func quoted(sql string, i int, q byte, escapes, triple, doubling bool) (int, error) {
	n := len(sql)
	if triple && i+2 < n && sql[i+1] == q && sql[i+2] == q {
		j := i + 3
		for j < n {
			switch {
			case sql[j] == '\\' && escapes:
				j += 2
			case sql[j] == q && j+2 < n && sql[j+1] == q && sql[j+2] == q:
				return j + 3, nil
			default:
				j++
			}
		}
		return 0, errUnterminatedString
	}
	j := i + 1
	for j < n {
		switch {
		case sql[j] == '\\' && escapes:
			j += 2
		case sql[j] == q:
			if doubling && j+1 < n && sql[j+1] == q {
				j += 2
				continue
			}
			return j + 1, nil
		default:
			j++
		}
	}
	return 0, errUnterminatedString
}

// dollarTag returns the "$tag$" opening a dollar-quoted literal at i, or ""
// if sql[i:] does not start one ("$1" is a positional parameter).
func dollarTag(sql string, i int) string {
	j := i + 1
	if j < len(sql) && sql[j] == '$' {
		return "$$"
	}
	if j >= len(sql) || !isIdentStart(sql[j]) {
		return ""
	}
	for j < len(sql) && (isIdentStart(sql[j]) || sql[j] >= '0' && sql[j] <= '9') {
		j++
	}
	if j < len(sql) && sql[j] == '$' {
		return sql[i : j+1]
	}
	return ""
}

// codeRanges calls fn for every code region [start, end) before limit.
func (s *scanned) codeRanges(limit int, fn func(start, end int)) {
	pos := 0
	for _, sp := range s.spans {
		if sp.start >= limit {
			break
		}
		if sp.start > pos {
			fn(pos, sp.start)
		}
		pos = sp.end
	}
	if pos < limit {
		fn(pos, limit)
	}
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// contentEnd returns the end of the statement's significant content: the
// position after the last literal, quoted identifier or code byte that is
// neither whitespace nor ';'. Trailing whitespace, comments and semicolons
// lie beyond it. It is 0 for an empty statement.
func (s *scanned) contentEnd() int {
	end := 0
	s.codeRanges(len(s.sql), func(a, b int) {
		for k := b - 1; k >= a; k-- {
			if c := s.sql[k]; !isSpace(c) && c != ';' {
				end = max(end, k+1)
				break
			}
		}
	})
	for _, sp := range s.spans {
		if sp.kind != spanComment {
			end = max(end, sp.end)
		}
	}
	return end
}

// leadingKeyword returns the first word of the statement after whitespace,
// comments and opening parentheses, upper-cased. It returns "" when the
// statement starts with anything else (a literal, an operator, …).
func (s *scanned) leadingKeyword() string {
	sql := s.sql
	i := 0
	si := 0
	for i < len(sql) {
		if si < len(s.spans) && s.spans[si].start == i {
			if s.spans[si].kind != spanComment {
				return ""
			}
			i = s.spans[si].end
			si++
			continue
		}
		c := sql[i]
		if isSpace(c) || c == '(' {
			i++
			continue
		}
		if !isIdentStart(c) {
			return ""
		}
		j := i
		for j < len(sql) && isIdentChar(sql[j]) {
			j++
		}
		return strings.ToUpper(sql[i:j])
	}
	return ""
}

// hasStatementSeparator reports whether a ';' occurs in code before limit.
func (s *scanned) hasStatementSeparator(limit int) bool {
	found := false
	s.codeRanges(limit, func(a, b int) {
		if !found && strings.IndexByte(s.sql[a:b], ';') >= 0 {
			found = true
		}
	})
	return found
}

// slot is a placeholder occurrence in a statement: [start, end) is replaced
// by the engine's positional placeholder; name is set for "@name" slots.
type slot struct {
	start, end int
	name       string
}

// questionSlots returns every "?" in code before limit.
func (s *scanned) questionSlots(limit int) []slot {
	var out []slot
	s.codeRanges(limit, func(a, b int) {
		for k := a; k < b; k++ {
			if s.sql[k] == '?' {
				out = append(out, slot{start: k, end: k + 1})
			}
		}
	})
	return out
}

// atSlots returns every "@name" in code before limit: an "@" not preceded by
// an identifier character or another "@" (so "@@system_var" and "a@b" are not
// placeholders), followed by an identifier.
func (s *scanned) atSlots(limit int) []slot {
	var out []slot
	sql := s.sql
	s.codeRanges(limit, func(a, b int) {
		for k := a; k < b; k++ {
			if sql[k] != '@' || k+1 >= b || !isIdentStart(sql[k+1]) || sql[k+1] >= 0x80 {
				continue
			}
			if k > 0 && (sql[k-1] == '@' || isIdentChar(sql[k-1])) {
				continue
			}
			j := k + 1
			for j < b && (isIdentStart(sql[j]) && sql[j] < 0x80 || sql[j] >= '0' && sql[j] <= '9') {
				j++
			}
			out = append(out, slot{start: k, end: j, name: sql[k+1 : j]})
			k = j - 1
		}
	})
	return out
}

// template is a statement split around its placeholder slots, ready to be
// rendered with an engine's placeholder syntax.
type template struct {
	parts []string // len(names)+1 literal pieces
	names []string // slot names ("" for "?" slots), in order of appearance
}

// newTemplate cuts sql[:limit] at slots (which must be sorted).
func newTemplate(sql string, limit int, slots []slot) template {
	t := template{parts: make([]string, 0, len(slots)+1), names: make([]string, 0, len(slots))}
	pos := 0
	for _, sl := range slots {
		t.parts = append(t.parts, sql[pos:sl.start])
		t.names = append(t.names, sl.name)
		pos = sl.end
	}
	t.parts = append(t.parts, sql[pos:limit])
	return t
}

// render joins the template using placeholder(i) for slot i.
func (t template) render(placeholder func(i int) string) string {
	var b strings.Builder
	for i, p := range t.parts {
		b.WriteString(p)
		if i < len(t.names) {
			b.WriteString(placeholder(i))
		}
	}
	return b.String()
}
