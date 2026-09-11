package httpclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCacheResponse(t *testing.T) {
	file, err := Render("client", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fixture := `package client
import("context";"encoding/pem";"net/http";"net/http/httptest";"os";"path/filepath";"strconv";"sync/atomic";"testing")
func TestCacheResponse(t *testing.T){
 var followed atomic.Int32
 server:=httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
 if r.URL.Path=="/redirect-target"{followed.Add(1);return}
 code,_:=strconv.Atoi(r.URL.Path[1:]);w.Header().Set("ETag", "record-tag");w.Header().Set("Location","/redirect-target");w.WriteHeader(code)
 }));defer server.Close()
 ca:=filepath.Join(t.TempDir(),"ca.pem");if err:=os.WriteFile(ca,pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE",Bytes:server.Certificate().Raw}),0600);err!=nil{t.Fatal(err)}
 client,err:=New(Options{BaseURL:server.URL,CAFile:ca});if err!=nil{t.Fatal(err)};defer client.Close()
 for _,method:=range []string{"GET","HEAD"}{result,err:=client.Do(context.Background(),method,"/304",http.Header{"If-None-Match":{"record-tag"}},nil);if err!=nil||result.StatusCode!=304||len(result.Body)!=0||result.Header.Get("ETag")!="record-tag"{t.Fatal("cache response changed",err)}}
 for _,code:=range []string{"300","301","302","303","305","307","308"}{if _,err:=client.Do(context.Background(),"GET","/"+code,nil,nil);err==nil{t.Fatal("redirect accepted",code)}}
 if _,err:=client.Do(context.Background(),"POST","/304",nil,nil);err==nil{t.Fatal("304 accepted for a mutation")}
 if followed.Load()!=0{t.Fatal("redirect was followed")}
}
`
	for name, data := range map[string][]byte{"go.mod": []byte("module example.com/cache-test\ngo 1.26.8\n"), "client.go": file.Bytes(), "client_test.go": []byte(fixture)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated cache response: %v\n%s", err, out)
	}
}
