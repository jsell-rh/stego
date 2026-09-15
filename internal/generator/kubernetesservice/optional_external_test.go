package kubernetesservice

import (
	"fmt"
	"testing"

	"github.com/jsell-rh/stego/internal/gen"
)

func TestOptionalExternalValidation(t *testing.T) {
	for _, scope := range []string{"service", "worker", "rpc"} {
		for name, mutate := range map[string]func(map[string]any){
			"type":   func(c map[string]any) { c["optional_external_endpoints"] = "probe" },
			"entry":  func(c map[string]any) { c["optional_external_endpoints"] = []any{42} },
			"name":   func(c map[string]any) { c["optional_external_endpoints"] = []any{"../probe"} },
			"repeat": func(c map[string]any) { c["optional_external_endpoints"] = []any{"probe", "probe"} },
			"overlap": func(c map[string]any) {
				c["external_endpoints"] = []any{"probe"}
				c["optional_external_endpoints"] = []any{"probe"}
			},
			"combined count": func(c map[string]any) {
				required := []any{}
				for i := 0; i < 32; i++ {
					required = append(required, fmt.Sprintf("endpoint-%d", i))
				}
				c["external_endpoints"] = required
				c["optional_external_endpoints"] = []any{"extra"}
			},
		} {
			t.Run(scope+"/"+name, func(t *testing.T) {
				ctx := rpcContext()
				c := ctx.ComponentConfig
				if scope == "worker" {
					c = c["workers"].([]any)[0].(map[string]any)
				}
				if scope == "rpc" {
					c = c["rpc_processes"].([]any)[0].(map[string]any)
				}
				mutate(c)
				if _, _, err := new(Generator).Generate(ctx); err == nil {
					t.Fatal("invalid optional declaration accepted")
				}
			})
		}
	}
	ctx := gen.Context{ComponentConfig: map[string]any{"external_endpoints": []any{"b", "a"}, "optional_external_endpoints": []any{"d", "c"}}}
	required, optional, err := endpointDeclarations(ctx)
	if err != nil || fmt.Sprint(required) != "[a b]" || fmt.Sprint(optional) != "[c d]" {
		t.Fatal("declaration order differs", err)
	}
}

const optionalExternalRendererTests = `
func TestOptionalExternalEndpoints(t *testing.T){
 base:=[]string{"--image","registry.example.test/team/widget@sha256:"+strings.Repeat("a",64),"--namespace","test"}
 for _,tc:=range []struct{name string;args []string;required int}{
  {"service-probe",nil,0},
  {"worker-probe",[]string{"--worker","remote","--egress","provider=192.0.2.10:5432"},1},
  {"rpc-probe",[]string{"--rpc-process","records"},0},
 }{
  t.Run(tc.name,func(t *testing.T){
   args:=append(append([]string{},base...),tc.args...)
   check:=func(extra []string,want int)[]byte{
    t.Helper();var output bytes.Buffer
    if err:=render(append(append([]string{},args...),extra...),&output);err!=nil{t.Fatal(err)}
    if strings.Contains(output.String(),"stego-external"){t.Fatal("unbound placeholder remains")}
    var doc struct{Items []map[string]any};if err:=json.Unmarshal(output.Bytes(),&doc);err!=nil{t.Fatal(err)}
    count:=0
    for _,item:=range doc.Items{if item["kind"]!="NetworkPolicy"{continue}
     for _,raw:=range item["spec"].(map[string]any)["egress"].([]any){
      rule:=raw.(map[string]any);peers:=rule["to"].([]any)
      if len(peers)!=1{t.Fatal("egress destination is too broad")}
      block,ok:=peers[0].(map[string]any)["ipBlock"];if !ok{continue};count++
      ports:=rule["ports"].([]any);if len(ports)!=1{t.Fatal("port rule is too broad")}
      port:=ports[0].(map[string]any);cidr:=block.(map[string]any)["cidr"]
      if port["protocol"]!="TCP"{t.Fatal("wrong protocol")}
      switch cidr{
      case "192.0.2.10/32":if tc.required!=1||port["port"]!=float64(5432){t.Fatal("required binding differs")}
      case "192.0.2.20/32":if port["port"]!=float64(443){t.Fatal("optional port differs")}
      case "2001:db8::20/128":if port["port"]!=float64(8443){t.Fatal("optional IPv6 port differs")}
      default:t.Fatal("unexpected external destination",cidr)
      }
     }
    }
    if count!=want{t.Fatal("wrong external rule count",count,want)}
    return output.Bytes()
   }
   check(nil,tc.required)
   one:=[]string{"--egress",tc.name+"=192.0.2.20:443"};two:=[]string{"--egress",tc.name+"=[2001:db8::20]:8443"}
   forward:=check(append(append([]string{},one...),two...),tc.required+2)
   reverse:=check(append(append([]string{},two...),one...),tc.required+2)
   if !bytes.Equal(forward,reverse){t.Fatal("binding order changes output")}
   for _,value:=range []string{tc.name+"=127.0.0.1:443",tc.name+"=169.254.169.254:443",tc.name+"=192.0.2.0/24:443","unknown=192.0.2.20:443"}{
    var output bytes.Buffer
    if err:=render(append(append([]string{},args...),"--egress",value),&output);err==nil||output.Len()!=0{t.Fatal("invalid optional binding emitted output")}
   }
   for _,other:=range []string{"service-probe","worker-probe","rpc-probe"}{if other==tc.name{continue};var output bytes.Buffer
    if err:=render(append(append([]string{},args...),"--egress",other+"=192.0.2.20:443"),&output);err==nil||output.Len()!=0{t.Fatal("binding crossed workload scope")}
   }
  })
 }
 var output bytes.Buffer
 if err:=render(append(append([]string{},base...),"--worker","remote","--egress","worker-probe=192.0.2.20:443"),&output);err==nil||output.Len()!=0{t.Fatal("optional binding bypassed required binding")}
 output.Reset()
 if err:=render(append(append([]string{},base...),"--egress","service-probe=192.0.2.20:443","--egress","service-probe=192.0.2.20:443"),&output);err==nil||output.Len()!=0{t.Fatal("duplicate optional binding accepted")}
}
`
