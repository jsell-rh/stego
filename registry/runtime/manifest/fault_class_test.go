package manifest

import (
	"slices"
	"testing"

	"github.com/jsell-rh/stego/registry/runtime/probe"
)

func TestFaultClass(t *testing.T) {
	t.Run("ST-SUP-RP15-LC01-T17 Each fault of Check has its class, and each class is a value of the field class of schema.manifest.mismatch", func(t *testing.T) {
		for fault, class := range map[string]string{
			"table public.orders: row-level security is not enabled and forced":                                                    "rls",
			"table public.orders: policy orders_tenant is missing":                                                                 "policy",
			"table public.orders: policy evil is a permissive policy that the manifest does not name":                              "policy",
			"table stego_outbox.messages: the privilege SELECT to PUBLIC is granted, and the manifest does not grant it":           "grant",
			"table public.orders: the privilege SELECT to svc_rt of the manifest is not granted":                                   "grant",
			"function public.audit_maintain may be run by roles that the manifest does not name":                                   "grant",
			"the confined role svc_relay holds a privilege on the relation public.orders, which the manifest does not name for it": "grant",
			"table public.orders: the table belongs to the role svc_relay, and the manifest needs svc_owner":                       "owner",
			"the confined role svc_relay owns the table public.x, which the manifest does not name for it":                         "owner",
			"the relation svc_rt.orders comes before public.orders on the search path of the session":                              "owner",
			"the schema svc_rt has the name of the role of the session, which a search path of \"$user\" puts first":               "owner",
			"table public.audit_event: trigger audit_event_reject_row is missing":                                                  "trigger",
			"the session runs with session_replication_role replica, which skips the triggers":                                     "trigger",
			"function public.audit_maintain is not the function of the manifest":                                                   "function",
			"table public.orders: rule r is a rule that the manifest does not allow":                                               "rule",
			"table public.orders: column status has the type character varying(20), and the manifest needs text":                   "column",
			"table public.orders: column status has a default that the manifest does not state":                                    "column",
			"table public.orders: index orders_pkey is missing":                                                                    "other",
			"table public.orders: constraint c is not validated":                                                                   "other",
			// A table name with the text of another class does not change the class: the class is read after it.
			"table public.\"policy x\": column a is missing": "column",
		} {
			if got := FaultClass(fault); got != class {
				t.Errorf("FaultClass(%q) = %q, want %q", fault, got, class)
			}
			if !slices.Contains(probe.SchemaFaultClasses, class) {
				t.Errorf("the class %q is not a value of the field class of schema.manifest.mismatch", class)
			}
		}
	})
}
