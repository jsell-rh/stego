package gosdk

const nullableRuntimeTest = `package sdk

import (
 "context"
 "crypto/tls"
 "encoding/json"
 "encoding/pem"
 "io"
 "net/http"
 "net/http/httptest"
 "os"
 "path/filepath"
 "testing"
)

func TestNullableExchange(t *testing.T) {
 t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
 received := make(chan []byte, 1)
 server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
  if r.Method != http.MethodPatch || r.URL.Path != "/widgets/one" { t.Error("wrong nullable request target") }
  body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
  if err != nil || len(body) > 4096 { t.Error("invalid nullable request"); w.WriteHeader(400); return }
  received <- body
  w.Header().Set("Content-Type", "application/json")
  w.Write(body)
 }))
 server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
 server.StartTLS()
 defer server.Close()
 ca := filepath.Join(t.TempDir(), "ca.pem")
 if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil { t.Fatal(err) }
 client, err := NewClient(Options{BaseURL: server.URL, CAFile: ca, Token: "test-token"})
 if err != nil { t.Fatal(err) }
 defer client.Close()
 for _, source := range []string{
  "{\"id\":\"one\",\"name\":\"Widget\"}",
  "{\"id\":\"one\",\"name\":\"Widget\",\"description\":null,\"enabled\":null,\"count\":null,\"updated\":null,\"tags\":null}",
  "{\"id\":\"one\",\"name\":\"Widget\",\"description\":\"\",\"enabled\":false,\"count\":0,\"updated\":\"2026-09-11T12:00:00Z\",\"tags\":[]}",
 } {
  var input PatchWidgetJSONRequestBody
  if err := json.Unmarshal([]byte(source), &input); err != nil { t.Fatal(err) }
  response, err := client.PatchWidgetWithResponse(context.Background(), "one", input)
  if err != nil || response.JSON200 == nil { t.Fatal("nullable exchange failed", err) }
  encoded, err := json.Marshal(response.JSON200)
  if err != nil { t.Fatal(err) }
  for _, actual := range [][]byte{<-received, encoded} {
   var want, got map[string]json.RawMessage
   if err := json.Unmarshal([]byte(source), &want); err != nil { t.Fatal(err) }
   if err := json.Unmarshal(actual, &got); err != nil { t.Fatal(err) }
   if len(want) != len(got) { t.Fatal("SDK changed field presence") }
   for key, value := range want { if string(value) != string(got[key]) { t.Fatalf("SDK changed nullable field %s", key) } }
  }
  if input.Description.IsSpecified() != response.JSON200.Description.IsSpecified() || input.Description.IsNull() != response.JSON200.Description.IsNull() { t.Fatal("SDK lost nullable state") }
 }
 var field Widget
 field.Description.SetNull()
 if !field.Description.IsNull() { t.Fatal("cannot set explicit null") }
 field.Description.Set("")
 if value, err := field.Description.Get(); err != nil || value != "" { t.Fatal("cannot set an empty value") }
 field.Description.SetUnspecified()
 if field.Description.IsSpecified() { t.Fatal("cannot omit field") }
}
`
