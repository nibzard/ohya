package mcpserver

import (
	"errors"
	"testing"
)

func TestSingleStatement(t *testing.T) {
	single := []string{
		`SELECT 1`,
		`SELECT 1;`,
		"SELECT 1;\n",
		`select 1; -- trailing comment`,
		`select 1 -- ; in comment`,
		`/* ; */ select 1`,
		`select ';'`,
		`select 'a'';b'`,
		`select "col;x"`,
		`select [col;x]`,
		"select `col;x`",
		`select x'de;ad'`,
		`select 1 /* ; /* nested-looking ; */ */`,
		`with cte as (select 1) select * from cte`,
		`select 1 ;
		 -- done
		`,
		"-- lead\nselect 1;\n-- tail",
	}
	for _, q := range single {
		if err := singleStatement(q); err != nil {
			t.Errorf("singleStatement(%q) = %v, want nil", q, err)
		}
	}

	multiple := []string{
		`select 1; select 2`,
		`select 1;;
		`,
		`;select 1`,
		`select 1; select 2; select 3`,
		`select ';' ; select 2`,
		`select 1; -- c
		select 2`,
		`insert into t values(1); drop table t`,
		`select 1
		; select 2`,
		`;;;`, // three empty statements
	}
	for _, q := range multiple {
		if err := singleStatement(q); !errors.Is(err, ErrMultipleStatements) {
			t.Errorf("singleStatement(%q) = %v, want ErrMultipleStatements", q, err)
		}
	}

	empty := []string{
		``,
		`   `,
		"\n\t ",
		`;`,
		`-- comment only`,
		`/* comment only */`,
		`/* comment */ ; /* another */`,
	}
	for _, q := range empty {
		if err := singleStatement(q); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("singleStatement(%q) = %v, want ErrEmptyQuery", q, err)
		}
	}
}

// FuzzSingleStatement checks the scanner never panics and stays stable:
// adding trailing whitespace or comments must not change a verdict.
func FuzzSingleStatement(f *testing.F) {
	for _, seed := range []string{
		`select 1`, `select 1; select 2`, `';'`, `"a""b"`, `/* ; */ x`, `-- ;`,
		`[a;b]`, "`x;`", `x'00;'`, `;;`, `; select 1`, `select 1;`,
	} {
		f.Add(seed)
	}
	classify := func(err error) string {
		switch {
		case err == nil:
			return "single"
		case errors.Is(err, ErrMultipleStatements):
			return "multiple"
		case errors.Is(err, ErrEmptyQuery):
			return "empty"
		default:
			return "other"
		}
	}
	f.Fuzz(func(t *testing.T, query string) {
		base := classify(singleStatement(query))
		for _, suffix := range []string{"", " ", "\n", " -- tail", " /* ; */"} {
			if got := classify(singleStatement(query + suffix)); got != base {
				t.Fatalf("trailing %q changed the verdict on %q: %s vs %s", suffix, query, got, base)
			}
		}
	})
}
