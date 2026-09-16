package keycloak

import (
	"context"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
)

func TestAccessAbnormalExit(t *testing.T) {
	for _, phase := range []string{"enabled inspection", "repair", "final inspection"} {
		for _, exit := range []string{"panic", "Goexit"} {
			t.Run(phase+"/"+exit, func(t *testing.T) {
				c, f, b, _ := newNativeAccessFixture(t)
				value := f.clients[b.ID]
				value.Enabled = phase == "enabled inspection"
				f.clients[b.ID] = value
				var reads atomic.Int32
				intercept := f.intercept
				f.intercept = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method == http.MethodGet && r.URL.Path == "/admin/realms/tenant/clients/worker" {
						reads.Add(1)
					}
					return intercept(w, r)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				marker := new(int)
				var readsAtExit int32
				abort := func() {
					readsAtExit = reads.Load()
					cancel()
					if exit == "panic" {
						panic(marker)
					}
					runtime.Goexit()
				}
				var recovered any
				returned := false
				finished := make(chan struct{})
				go func() {
					defer close(finished)
					defer func() { recovered = recover() }()
					work, done, err := c.begin(ctx)
					if err != nil {
						panic(err)
					}
					defer done()
					_ = c.reconcileClientAccess(work, b, clientAccessPlan{
						inspect: func(_ context.Context, enabled bool) error {
							if enabled {
								abort()
							}
							return nil
						},
						repair: func(context.Context) error {
							if phase == "repair" {
								abort()
							}
							return nil
						},
					})
					returned = true
				}()
				<-finished
				if returned {
					t.Fatal("abnormal exit returned normally")
				}
				if exit == "panic" && recovered != marker {
					t.Fatal("panic was replaced or suppressed")
				}
				if exit == "Goexit" && recovered != nil {
					t.Fatal("Goexit became a panic")
				}
				if f.clients[b.ID].Enabled {
					t.Fatal("abnormal exit left the client enabled")
				}
				if reads.Load() <= readsAtExit {
					t.Fatal("abnormal exit did not confirm the saved binding")
				}
				if phase != "repair" && (len(f.writes) == 0 || f.writes[len(f.writes)-1] != "PUT enabled=false") {
					t.Fatal("abnormal exit did not attempt disablement")
				}
				if len(c.permits) != 0 {
					t.Fatal("abnormal exit retained an operation permit")
				}
			})
		}
	}
}
