package search

import (
	"reflect"
	"strings"
	"sync"
	"testing"
)

func BenchmarkSearch(b *testing.B) {
	engine := NewSearchEngine()
	input := "(name like 'a%' and serial > 100) or (role in ('admin','member') and created_at > 2020-01-01)"
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := engine.ParseSearch("User", input); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchParallel(b *testing.B) {
	engine := NewSearchEngine()
	input := "(name like 'a%' and serial > 100) or (role in ('admin','member') and created_at > 2020-01-01)"
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := engine.ParseSearch("User", input); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func TestBoundSearch(t *testing.T) {
	engine := NewSearchEngine()
	t.Run("ST-SUP-RC23-OC07-T01 The emitted search engine stays correct under concurrent use", func(t *testing.T) {
		var cold sync.WaitGroup
		for i := range 16 {
			cold.Go(func() {
				input := []string{"serial between -10 and 20", "created_at > 2020-01-01", "name not in ('first','second')", "name ilike 'x%' or name is null"}[i%4]
				if _, err := engine.ParseSearch("User", input); err != nil {
					t.Error(err)
				}
			})
		}
		cold.Wait()
	})
	type searchCase struct {
		input, where string
		args         []interface{}
	}
	valid := func(t *testing.T, tests []searchCase) {
		for _, test := range tests {
			got, err := engine.ParseSearch("User", test.input)
			if err != nil {
				t.Fatalf("%s: %v", test.input, err)
			}
			if strings.Count(got.Where, "?") != len(got.Args) {
				t.Fatalf("parameter count differs: %s %#v", got.Where, got.Args)
			}
			if got.Where != test.where {
				t.Fatalf("%s: SQL=%s want=%s", test.input, got.Where, test.where)
			}
			if test.args != nil && !reflect.DeepEqual(got.Args, test.args) {
				t.Fatalf("%s: args=%#v want=%#v", test.input, got.Args, test.args)
			}
		}
	}
	invalid := func(t *testing.T, inputs []string) {
		for _, input := range inputs {
			if _, err := engine.ParseSearch("User", input); err == nil {
				t.Fatalf("invalid search accepted: %s", input)
			}
		}
	}
	t.Run("ST-SUP-RC23-OC02-T01 The emitted search engine quotes every column and binds every value", func(t *testing.T) {
		valid(t, []searchCase{
			{"name = 'alice'", `("name" = ?)`, []interface{}{"alice"}},
			{"id in ('a','b')", `("id" IN (?,?))`, []interface{}{"a", "b"}},
			{"name is null", `("name" IS NULL)`, nil},
			{"name = 'x'' OR TRUE --'", `("name" = ?)`, []interface{}{"x' OR TRUE --"}},
		})
	})
	t.Run("ST-SUP-RC23-OC03-T01 The emitted search engine keeps explicit parentheses", func(t *testing.T) {
		valid(t, []searchCase{
			{"name = 'alice' or role = 'admin'", `(("name" = ?) OR ("role" = ?))`, []interface{}{"alice", "admin"}},
			{"name not like 'x%'", `(NOT ("name" LIKE ?))`, []interface{}{"x%"}},
			{"serial + 1 = 2", `(("serial" + ?) = ?)`, []interface{}{"1", "2"}},
		})
	})
	t.Run("ST-SUP-RC23-OC06-T01 The emitted search engine turns an empty list into a constant", func(t *testing.T) {
		valid(t, []searchCase{
			{"serial + 1 in ()", `FALSE`, nil},
			{"serial + 1 not in ()", `TRUE`, nil},
		})
	})
	t.Run("ST-SUP-RC23-OC05-T01 The emitted search engine keeps the numeric text", func(t *testing.T) {
		valid(t, []searchCase{
			{"serial = 9007199254740993", `("serial" = ?)`, []interface{}{"9007199254740993"}},
			{"serial between -10 and 2Ki", `("serial" BETWEEN ? AND ?)`, []interface{}{"-10", "2048"}},
			{"serial = -1e-999", `("serial" = ?)`, []interface{}{"-1e-999"}},
		})
	})
	t.Run("ST-SUP-RC23-OC01-T01 The emitted search engine maps each field to a declared column and refuses an unknown field", func(t *testing.T) {
		valid(t, []searchCase{
			{"created_at = 2026-09-08", `("created_time" = ?)`, nil},
		})
		invalid(t, []string{"missing = 'x'", "name = 'a' or missing = 'b'", "name + missing = 3", `"name) OR TRUE --" = 'x'`})
	})
	t.Run("ST-SUP-RC23-OC04-T01 The emitted search engine holds its input limits", func(t *testing.T) {
		invalid(t, []string{strings.Repeat("(", 33) + "name='x'" + strings.Repeat(")", 33), strings.Repeat("not ", 100) + "name='x'", strings.Repeat("x", 4097)})
	})
	invalid(t, []string{"name='x';SELECT 1", "name='\x00'"})
	t.Run("ST-SUP-RC23-OC07-T01 The emitted search engine stays correct under concurrent use", func(t *testing.T) {
		var wait sync.WaitGroup
		for range 16 {
			wait.Go(func() {
				for range 20 {
					if _, err := engine.ParseSearch("User", "name = 'safe'"); err != nil {
						t.Error(err)
					}
				}
			})
		}
		wait.Wait()
	})
}
