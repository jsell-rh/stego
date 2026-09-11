package kubernetesservice

const externalRendererTests = `
func TestExternalEndpointPolicy(t *testing.T){
 base:=[]string{"--image","registry.example.test/team/widget@sha256:"+strings.Repeat("a",64),"--namespace","test","--worker","remote"}
 one:=[]string{"--egress","provider=192.0.2.10:6443"}
 two:=[]string{"--egress","provider=[2001:db8::1]:443"}
 args:=append(append(append([]string{},base...),one...),two...)
 var output bytes.Buffer;if err:=render(args,&output);err!=nil{t.Fatal(err)}
 var doc struct{Items []map[string]any};if err:=json.Unmarshal(output.Bytes(),&doc);err!=nil{t.Fatal(err)}
 found:=map[string]float64{}
 for _,item:=range doc.Items{
  if item["kind"]!="NetworkPolicy"{continue}
  spec:=item["spec"].(map[string]any)
  if len(spec["ingress"].([]any))!=0{t.Fatal("external binding added ingress")}
  for _,raw:=range spec["egress"].([]any){
   rule:=raw.(map[string]any);peers:=rule["to"].([]any)
   block,ok:=peers[0].(map[string]any)["ipBlock"];if !ok{continue}
   ports:=rule["ports"].([]any);if len(peers)!=1||len(ports)!=1{t.Fatal("endpoint rule is too broad")}
   port:=ports[0].(map[string]any);if port["protocol"]!="TCP"{t.Fatal("endpoint protocol differs")}
   found[block.(map[string]any)["cidr"].(string)]=port["port"].(float64)
  }
 }
 if len(found)!=2||found["192.0.2.10/32"]!=6443||found["2001:db8::1/128"]!=443{t.Fatal("endpoint address and port pairs differ",found)}
 var reversed bytes.Buffer
 args=append(append(append([]string{},base...),two...),one...)
 if err:=render(args,&reversed);err!=nil||!bytes.Equal(output.Bytes(),reversed.Bytes()){t.Fatal("binding order changed output",err)}
 for _,extra:=range [][]string{
  nil,{"--egress","unknown=192.0.2.10:443"},
  {"--egress","provider=private-kube.invalid:443"},
  {"--egress","provider=192.0.2.0/24:443"},
  {"--egress","provider=0.0.0.0:443"},
  {"--egress","provider=127.0.0.1:443"},
  {"--egress","provider=169.254.169.254:443"},
  {"--egress","provider=224.0.0.1:443"},
  {"--egress","provider=[::]:443"},
  {"--egress","provider=[::1]:443"},
  {"--egress","provider=[::ffff:192.0.2.10]:443"},
  {"--egress","provider=[2001:db8::1%eth0]:443"},
  {"--egress","provider=192.0.2.10:0"},
  {"--egress","provider=192.0.2.10:65536"},
  {"--egress","provider=192.0.2.10:443","--egress","provider=192.0.2.10:443"},
  {"--egress","provider=192.0.2.10:443","--egress","extra=192.0.2.10:443"},
 }{
  output.Reset();err:=render(append(append([]string{},base...),extra...),&output)
  if err==nil||output.Len()!=0{t.Fatal("invalid endpoint emitted a manifest")}
  if strings.Contains(err.Error(),"private-kube"){t.Fatal("endpoint error exposed input")}
 }
 output.Reset()
 if err:=render(append(append([]string{},base[:len(base)-2]...),one...),&output);err==nil||output.Len()!=0{t.Fatal("service accepted a worker-only endpoint")}
 extra:=append([]string{},base...);for i:=0;i<33;i++{extra=append(extra,one...)}
 output.Reset();if err:=render(extra,&output);err==nil||output.Len()!=0{t.Fatal("endpoint limit was not enforced")}
}
`
