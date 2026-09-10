package command

import (
 "bytes"
 "context"
 "encoding/json"
 "testing"
)

func TestVersionWithoutConfiguration(t *testing.T) {
 t.Setenv("VERSION_TEST_CONFIG","/missing/config.json")
 app:=Application{ConfigEnv:"VERSION_TEST_CONFIG",ConfigName:"version-test",VersionCommand:true}
 var out bytes.Buffer
 if err:=Run(context.Background(),app,[]string{"version"},&out);err!=nil{t.Fatal(err)}
 var report map[string]json.RawMessage
 if err:=json.Unmarshal(out.Bytes(),&report);err!=nil||len(report)!=2||report["compiler"]==nil||report["application"]==nil{t.Fatal(out.String(),err)}
 if err:=CompilerBuild().Validate();err!=nil{t.Fatal(err)}
 out.Reset()
 if err:=Run(context.Background(),app,[]string{"version","extra"},&out);err==nil||out.Len()!=0{t.Fatal("invalid arguments accepted")}
 app.Commands=[]Command{ {Name:[]string{"version"},Method:"GET",Path:"/version",Success:[]int{200}}}
 if validate(app)==nil{t.Fatal("reserved command accepted")}
 app.VersionCommand=false
 if err:=validate(app);err!=nil{t.Fatal("custom version command rejected",err)}
}
