// Package tenant is the tenant wall of the runtime library (ST-SUP-RP04): the Tenant type, the context accessor, the
// transaction helper that sets the tenant of the transaction, the tenant list with Provision and Retire, the iterator
// over tenants for workers and controllers, the connector of the owner role for migrations and the guard for pooler
// compatibility.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package tenant
