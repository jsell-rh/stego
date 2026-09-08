package search

import (
 "reflect"
 "strings"
 "sync"
 "testing"
)

func BenchmarkSearch(b *testing.B){
 engine:=NewSearchEngine();input:="(name like 'a%' and serial > 100) or (role in ('admin','member') and created_at > 2020-01-01)"
 b.ReportAllocs();b.ResetTimer()
 for range b.N{if _,err:=engine.ParseSearch("User",input);err!=nil{b.Fatal(err)}}
}

func BenchmarkSearchParallel(b *testing.B){
 engine:=NewSearchEngine();input:="(name like 'a%' and serial > 100) or (role in ('admin','member') and created_at > 2020-01-01)"
 b.ReportAllocs();b.ResetTimer()
 b.RunParallel(func(pb *testing.PB){for pb.Next(){if _,err:=engine.ParseSearch("User",input);err!=nil{b.Error(err);return}}})
}

func TestBoundSearch(t *testing.T){
 engine:=NewSearchEngine()
 var cold sync.WaitGroup
 for i:=range 16{cold.Go(func(){input:=[]string{"serial between -10 and 20","created_at > 2020-01-01","name not in ('first','second')","name ilike 'x%' or name is null"}[i%4];if _,err:=engine.ParseSearch("User",input);err!=nil{t.Error(err)}})};cold.Wait()
 for _,test:=range []struct{input,where string;args []interface{}}{
  {"name = 'alice'",`("name" = ?)`,[]interface{}{"alice"}},
  {"name = 'alice' or role = 'admin'",`(("name" = ?) OR ("role" = ?))`,[]interface{}{"alice","admin"}},
  {"serial + 1 in ()",`FALSE`,nil},
  {"serial + 1 not in ()",`TRUE`,nil},
  {"id in ('a','b')",`("id" IN (?,?))`,[]interface{}{"a","b"}},
  {"name is null",`("name" IS NULL)`,nil},
  {"name not like 'x%'",`(NOT ("name" LIKE ?))`,[]interface{}{"x%"}},
  {"serial + 1 = 2",`(("serial" + ?) = ?)`,[]interface{}{"1","2"}},
  {"name = 'x'' OR TRUE --'",`("name" = ?)`,[]interface{}{"x' OR TRUE --"}},
  {"serial = 9007199254740993",`("serial" = ?)`,[]interface{}{"9007199254740993"}},
  {"serial between -10 and 2Ki",`("serial" BETWEEN ? AND ?)`,[]interface{}{"-10","2048"}},
  {"serial = -1e-999",`("serial" = ?)`,[]interface{}{"-1e-999"}},
  {"created_at = 2026-09-08",`("created_time" = ?)`,nil},
 }{
  got,err:=engine.ParseSearch("User",test.input);if err!=nil{t.Fatalf("%s: %v",test.input,err)}
  if strings.Count(got.Where,"?")!=len(got.Args){t.Fatalf("parameter count differs: %s %#v",got.Where,got.Args)}
  if got.Where!=test.where{t.Fatalf("%s: SQL=%s want=%s",test.input,got.Where,test.where)}
  if test.args!=nil&&!reflect.DeepEqual(got.Args,test.args){t.Fatalf("%s: args=%#v want=%#v",test.input,got.Args,test.args)}
 }
 for _,input:=range []string{"missing = 'x'","name = 'a' or missing = 'b'","name + missing = 3",`"name) OR TRUE --" = 'x'`,"name='x';SELECT 1",strings.Repeat("(",33)+"name='x'"+strings.Repeat(")",33),strings.Repeat("not ",100)+"name='x'",strings.Repeat("x",4097),"name='\x00'"}{
  if _,err:=engine.ParseSearch("User",input);err==nil{t.Fatalf("invalid search accepted: %s",input)}
 }
 var wait sync.WaitGroup;for range 16{wait.Go(func(){for range 20{if _,err:=engine.ParseSearch("User","name = 'safe'");err!=nil{t.Error(err)}}})};wait.Wait()
}
