package httpclient

import (
	"context"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

//go:embed testdata/isolation_test.go
var isolationTests []byte

//go:embed testdata/options_test.go
var optionTests []byte

func TestStreamRequestIsolation(t *testing.T) { testStreams(t, false) }
func TestStreamLimitOptions(t *testing.T)     { testStreams(t, true) }

func testStreams(t *testing.T, options bool) {
	t.Helper()
	file, err := Render("client", "", FollowLinks)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string][]byte{"go.mod": []byte("module example.com/stream-test\ngo 1.26.8\n"), "client.go": file.Bytes(), "isolation_test.go": isolationTests}
	pattern := "^(TestStreamRequestIsolation|TestStreamCallbackOwnsItsSlot)$"
	if options {
		files["options_test.go"] = optionTests
		pattern = "^(TestStreamLimitOptions|TestConfiguredStreamLimitEnforced)$"
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-v", "-race", "-count=1", "-mod=readonly", "-timeout=30s", "-run="+pattern, "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated stream limits: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}

// Proves ST-SUP-RC10-OC27.
func TestCredentialLinkPolicy(t *testing.T) {
	follow, err := Render("follow", "", FollowLinks)
	if err != nil {
		t.Fatal(err)
	}
	refuse, err := Render("refuse", "", RefuseLinks)
	if err != nil {
		t.Fatal(err)
	}
	fixture := `package policy
import("os";"path/filepath";"testing";"example.com/link-test/follow";"example.com/link-test/refuse")
func TestLeafSymbolicLink(t *testing.T){
 directory:=t.TempDir()
 secret:=filepath.Join(directory,"secret")
 if err:=os.WriteFile(secret,[]byte("value"),0600);err!=nil{t.Fatal(err)}
 // A Kubernetes projected Secret mount is a leaf symbolic link into "..data".
 link:=filepath.Join(directory,"projected")
 if err:=os.Symlink(secret,link);err!=nil{t.Fatal(err)}
 if data,err:=follow.ReadPrivateFile(link);err!=nil||string(data)!="value"{t.Fatal("the default refused a projected credential file",err)}
 if _,err:=refuse.ReadPrivateFile(link);err==nil{t.Fatal("the refusing client read a symbolic link")}
 if data,err:=refuse.ReadPrivateFile(secret);err!=nil||string(data)!="value"{t.Fatal("the refusing client rejected a regular file",err)}
}
`
	dir := t.TempDir()
	files := map[string][]byte{
		"go.mod":           []byte("module example.com/link-test\ngo 1.26.8\n"),
		"follow/client.go": follow.Bytes(),
		"refuse/client.go": refuse.Bytes(),
		"policy_test.go":   []byte(fixture),
	}
	for name, data := range files {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-mod=readonly", "-timeout=30s", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated credential link policy: %v\n%s", err, output)
	}
}

func TestCacheResponse(t *testing.T) {
	file, err := Render("client", "", FollowLinks)
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
