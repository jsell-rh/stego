// Package authn is the authentication runtime of the runtime library (ST-SUP-RP13): the verifier of a JWT bearer
// token against a JSON Web Key Set, the key set and its remote source, the mapping of the verified claims to the
// principal, the roles and the tenant claim, and the bearer middleware. The verified identity it gives is the only
// Identity that the tenant runtime accepts.
//
// It is a library, not a framework (ST-STD-RUNTIME-R01): generated code calls its functions and types, and the
// package calls back into no generated code and owns no main loop.
package authn
