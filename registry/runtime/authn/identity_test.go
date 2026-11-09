package authn

import (
	"context"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"stego.dev/runtime/authn/authntest"
	"stego.dev/runtime/tenant"
)

// One file set and one source importer serve every type-check, so the imports are read once.
var (
	checkFiles    = token.NewFileSet()
	checkImporter = importer.ForCompiler(checkFiles, "source", nil)
)

// typeCheck type-checks the package source files that are not tests, with go/types.
func typeCheck(t *testing.T) *types.Package {
	t.Helper()
	fset := checkFiles
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	config := types.Config{Importer: checkImporter}
	pkg, err := config.Check("stego.dev/runtime/authn", fset, files, nil)
	if err != nil {
		t.Fatalf("type-check the package: %v", err)
	}
	return pkg
}

// checkOutside type-checks one file of a package outside the runtime module.
func checkOutside(t *testing.T, src string) error {
	t.Helper()
	fset := checkFiles
	file, err := parser.ParseFile(fset, "outside.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	config := types.Config{Importer: checkImporter}
	_, err = config.Check("example.com/outside", fset, []*ast.File{file}, nil)
	return err
}

func returnsIdentity(sig *types.Signature, identity types.Type) bool {
	for i := 0; i < sig.Results().Len(); i++ {
		if types.Identical(sig.Results().At(i).Type(), identity) {
			return true
		}
	}
	return false
}

func TestIdentitySeal(t *testing.T) {
	t.Run("ST-SUP-RP13-LC01-T01 Only Verify makes an Identity, and the Identity is sealed", func(t *testing.T) {
		pkg := typeCheck(t)
		scope := pkg.Scope()
		identity := scope.Lookup("Identity").Type()
		var givers []string
		for _, name := range scope.Names() {
			object := scope.Lookup(name)
			if !object.Exported() {
				continue
			}
			if fn, ok := object.(*types.Func); ok && returnsIdentity(fn.Type().(*types.Signature), identity) {
				givers = append(givers, name)
			}
			if typeName, ok := object.(*types.TypeName); ok {
				methods := types.NewMethodSet(types.NewPointer(typeName.Type()))
				for i := 0; i < methods.Len(); i++ {
					fn := methods.At(i).Obj().(*types.Func)
					if fn.Exported() && returnsIdentity(fn.Type().(*types.Signature), identity) {
						givers = append(givers, name+"."+fn.Name())
					}
				}
			}
		}
		// FromContext makes no Identity: it gives back the one that Middleware put in the context after Verify,
		// under an unexported key; ST-SUP-RP13-LC14-T01 proves that.
		if want := []string{"FromContext", "Verifier.Verify"}; !slices.Equal(givers, want) {
			t.Errorf("the functions that give an Identity are %v, want %v", givers, want)
		}
		structType := identity.Underlying().(*types.Struct)
		for i := 0; i < structType.NumFields(); i++ {
			if structType.Field(i).Exported() {
				t.Errorf("Identity has the exported field %s", structType.Field(i).Name())
			}
		}
		methods := types.NewMethodSet(types.NewPointer(identity))
		for i := 0; i < methods.Len(); i++ {
			fn := methods.At(i).Obj().(*types.Func)
			sig := fn.Type().(*types.Signature)
			if _, pointer := sig.Recv().Type().(*types.Pointer); pointer || sig.Params().Len() != 0 {
				t.Errorf("Identity has the method %s, which can change it", fn.Name())
			}
		}

		if err := checkOutside(t, `package outside

import (
	"stego.dev/runtime/authn"
	"stego.dev/runtime/tenant"
)

func use() { _ = tenant.Claim(authn.Identity{}) }
`); err != nil {
			t.Fatalf("the control package does not type-check: %v", err)
		}
		err := checkOutside(t, `package outside

import "stego.dev/runtime/tenant"

type identity struct{}

func (identity) TenantClaim() (string, bool) { return "t9", true }

func use() { _ = tenant.Claim(identity{}) }
`)
		if err == nil || !strings.Contains(err.Error(), "sealedIdentity") {
			t.Errorf("an outside type with the exported methods of Identity gave %v, want the missing seal method", err)
		}
		err = checkOutside(t, `package outside

import "stego.dev/runtime/authn"

var forged = authn.Identity{tenant: "t9"}
`)
		if err == nil {
			t.Error("an outside package set a field of Identity")
		}
		// The one Identity that an outside package can write, the zero value, holds no tenant.
		if _, err := tenant.New(tenant.Claim(Identity{})); err == nil {
			t.Error("tenant.New accepted the zero Identity")
		}
	})

	t.Run("ST-SUP-RP13-LC01-T02 tenant.Claim accepts a verified identity", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		id := expectAccepted(t, v, authntest.Mint(t, authntest.Header("k1"), goodClaims(), key))
		got, err := tenant.New(tenant.Claim(id))
		if err != nil {
			t.Fatalf("tenant.New: %v", err)
		}
		if got.ID() != "t1" {
			t.Errorf("the tenant is %q, want t1", got.ID())
		}
	})
}

func TestIdentityClaims(t *testing.T) {
	t.Run("ST-SUP-RP13-LC13-T01 Roles and tenant come only from their configured paths", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		header := authntest.Header("k1")
		roles := func(n int) []string {
			out := make([]string, n)
			for i := range out {
				out[i] = "role-" + strings.Repeat("r", i%8) + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
			}
			return out
		}
		long := strings.Repeat("r", MaxRoleBytes)
		good := goodClaims()
		good["realm_access"] = map[string]any{"roles": append(roles(MaxRoles-1), long)}
		id := expectAccepted(t, v, authntest.Mint(t, header, good, key))
		if want := (Principal{Issuer: testIssuer, Subject: "user-1"}); id.Principal() != want {
			t.Errorf("the principal is %+v, want %+v", id.Principal(), want)
		}
		if id.Issuer() != testIssuer || id.UserID() != "user-1" {
			t.Errorf("the issuer and user ID are %q and %q", id.Issuer(), id.UserID())
		}
		if got := id.Roles(); len(got) != MaxRoles || got[MaxRoles-1] != long {
			t.Errorf("the identity has %d roles, want %d", len(got), MaxRoles)
		}
		if tenantID, ok := id.TenantClaim(); !ok || tenantID != "t1" {
			t.Errorf("the tenant claim is %q, %v", tenantID, ok)
		}
		// A role path that is absent from the token gives no roles, with no fallback to another claim.
		for name, change := range map[string]func(map[string]any){
			"no role claim":                     func(c map[string]any) { delete(c, "realm_access") },
			"roles under another claim only":    func(c map[string]any) { delete(c, "realm_access"); c["roles"] = []string{"admin"} },
			"an object with no roles member":    func(c map[string]any) { c["realm_access"] = map[string]any{"groups": []string{"admin"}} },
			"roles one level up, not an object": func(c map[string]any) { c["realm_access"] = []string{"admin"} },
		} {
			t.Run(name, func(t *testing.T) {
				claims := goodClaims()
				change(claims)
				id := expectAccepted(t, v, authntest.Mint(t, header, claims, key))
				if id.Roles() != nil {
					t.Errorf("the identity has the roles %v, want none", id.Roles())
				}
			})
		}
		cases := []struct {
			name   string
			change func(map[string]any)
			want   error
		}{
			{"a role claim that is a string", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": "admin"} }, errRoles},
			{"129 roles", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": roles(MaxRoles + 1)} }, errRoles},
			{"a role of 257 bytes", func(c map[string]any) {
				c["realm_access"] = map[string]any{"roles": []string{"reader", long + "r"}}
			}, errRoles},
			{"a role with NUL", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": []string{"ad\x00min"}} }, errRoles},
			{"a role with U+202E", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": []string{"admin\u202eresu"}} }, errRoles},
			{"a role with U+2029", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": []string{"ad\u2029min"}} }, errRoles},
			{"a tenant with U+200B", func(c map[string]any) { c["tenant"] = "t\u200b1" }, errTenant},
			{"a tenant with U+2028", func(c map[string]any) { c["tenant"] = "t\u20281" }, errTenant},
			{"a role that is a number", func(c map[string]any) { c["realm_access"] = map[string]any{"roles": []any{"reader", 7}} }, errRoles},
			{"a tenant claim that is an array", func(c map[string]any) { c["tenant"] = []string{"t1"} }, errTenant},
			{"no tenant claim", func(c map[string]any) {
				delete(c, "tenant")
				c["tenant_id"] = "t1"
			}, errTenant},
			{"a tenant of 256 bytes", func(c map[string]any) { c["tenant"] = strings.Repeat("t", MaxTenantBytes+1) }, errTenant},
			{"an empty tenant", func(c map[string]any) { c["tenant"] = "" }, errTenant},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				claims := goodClaims()
				c.change(claims)
				id, err := v.Verify(context.Background(), authntest.Mint(t, header, claims, key))
				expectRefused(t, id, err, c.want)
			})
		}
		// With no paths, the identity has no roles and no tenant claim, whatever the token holds.
		bare := staticVerifier(t, func(s *Settings) { s.RoleClaim, s.TenantClaim = "", "" }, key)
		id = expectAccepted(t, bare, authntest.Mint(t, header, goodClaims(), key))
		if _, ok := id.TenantClaim(); ok || id.Roles() != nil {
			t.Errorf("a verifier with no claim paths gave roles %v and a tenant claim", id.Roles())
		}
	})
}

// forged is the attack of the security review of PR #144: a type that embeds a verified Identity and declares its
// exported methods again. The check below type-checks the same declaration outside the runtime module.
type forged struct{ Identity }

func (forged) TenantClaim() (string, bool) { return "t2", true }
func (forged) UserID() string              { return "admin" }
func (forged) Roles() []string             { return []string{"admin"} }

func TestIdentityEmbedding(t *testing.T) {
	t.Run("ST-SUP-RP13-LC01-T03 Embedding a verified Identity and declaring its methods again changes nothing", func(t *testing.T) {
		if err := checkOutside(t, `package outside

import (
	"stego.dev/runtime/authn"
	"stego.dev/runtime/tenant"
)

type forged struct{ authn.Identity }

func (forged) TenantClaim() (string, bool) { return "t2", true }
func (forged) UserID() string              { return "admin" }
func (forged) Roles() []string             { return []string{"admin"} }

func use(id authn.Identity) { _ = tenant.Claim(forged{id}) }
`); err != nil {
			t.Fatalf("the forged type does not type-check outside the module, so the case is not the attack: %v", err)
		}
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, nil, key)
		token := authntest.Mint(t, authntest.Header("k1"), goodClaims(), key)
		genuine := expectAccepted(t, v, token)
		got, err := tenant.New(tenant.Claim(forged{genuine}))
		if err != nil || got.ID() != "t1" {
			t.Errorf("tenant.Claim of the forged type gave %v, %v, want t1", got, err)
		}
		if _, err := tenant.New(tenant.Claim(forged{})); err == nil {
			t.Error("tenant.Claim of a forged type with a zero Identity gave a tenant")
		}
		// The context holds only the Identity that Verify made, under a key that no other package can name.
		var seen Identity
		handler := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), struct{}{}, forged{genuine})
			seen, _ = FromContext(ctx)
		}))
		request := httptest.NewRequest(http.MethodGet, "/orders", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		handler.ServeHTTP(httptest.NewRecorder(), request)
		if tenantID, _ := seen.TenantClaim(); seen.UserID() != "user-1" || tenantID != "t1" || slices.Contains(seen.Roles(), "admin") {
			t.Errorf("FromContext gave user %q, tenant %q and roles %v", seen.UserID(), tenantID, seen.Roles())
		}
		if _, ok := FromContext(context.WithValue(context.Background(), struct{}{}, forged{genuine})); ok {
			t.Error("FromContext gave an Identity that no middleware put in the context")
		}
	})
}

func TestIdentityProfile(t *testing.T) {
	t.Run("ST-SUP-RP13-LC18-T01 The profile claims reach the Identity", func(t *testing.T) {
		key := authntest.NewKey(t, "k1")
		v := staticVerifier(t, func(s *Settings) { s.AttributeClaims = []string{"department"} }, key)
		header := authntest.Header("k1")
		claims := goodClaims()
		claims["preferred_username"], claims["email"] = "jdoe", "jdoe@example.com"
		claims["given_name"], claims["family_name"], claims["department"] = "Jane", "Doe", "sales"
		claims["other"] = "not configured"
		id := expectAccepted(t, v, authntest.Mint(t, header, claims, key))
		if id.Username() != "jdoe" || id.Email() != "jdoe@example.com" || id.GivenName() != "Jane" || id.FamilyName() != "Doe" {
			t.Errorf("the profile is %q, %q, %q, %q", id.Username(), id.Email(), id.GivenName(), id.FamilyName())
		}
		if attributes := id.Attributes(); len(attributes) != 1 || attributes["department"] != "sales" {
			t.Errorf("the attributes are %v, want only department", attributes)
		}
		none := expectAccepted(t, v, authntest.Mint(t, header, goodClaims(), key))
		if none.Username() != "" || none.Email() != "" || none.GivenName() != "" || none.FamilyName() != "" || none.Attributes() != nil {
			t.Errorf("a token with no profile claims gave %+v", none)
		}
		for name, change := range map[string]func(map[string]any){
			"an email that is a number":   func(c map[string]any) { c["email"] = 7 },
			"a username with NUL":         func(c map[string]any) { c["preferred_username"] = "j\x00doe" },
			"a username with U+202E":      func(c map[string]any) { c["preferred_username"] = "j\u202eeod" },
			"an email with U+200B":        func(c map[string]any) { c["email"] = "jdoe\u200b@example.com" },
			"a family name with U+2028":   func(c map[string]any) { c["family_name"] = "Do\u2028e" },
			"a given name of 257 bytes":   func(c map[string]any) { c["given_name"] = strings.Repeat("j", MaxProfileBytes+1) },
			"an attribute that is a list": func(c map[string]any) { c["department"] = []string{"sales"} },
		} {
			t.Run(name, func(t *testing.T) {
				c := goodClaims()
				change(c)
				id, err := v.Verify(context.Background(), authntest.Mint(t, header, c, key))
				expectRefused(t, id, err, errProfile)
			})
		}
	})
}
