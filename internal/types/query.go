package types

// Query is one read that the service's own code carries out (ST-DECL-EN19): its name, the entities it reads, the
// permission it needs, the one fill that carries it out with a read-only query handle, the route and the refusals
// it may answer with.
type Query struct {
	// Name is the query's name, unique in the declaration and not the name of a command, such as open-reservations.
	Name string `yaml:"name"`
	// Reads names the entities that the query handle may read.
	Reads []string `yaml:"reads"`
	// ResourceType is the type in the authorization schema that the query acts on.
	ResourceType string `yaml:"resource_type,omitempty"`
	// Permission is the permission the caller needs on the resource before the fill runs.
	Permission string `yaml:"permission,omitempty"`
	// Fill names the one fill that carries out the query at the query slot.
	Fill FillNames `yaml:"fill"`
	// ResourceMember is the request member that holds the ID of the one resource the query is decided on. A list
	// query names none.
	ResourceMember string `yaml:"resource_member,omitempty"`
	// Route is the method and path the query is served at under the base path, such as GET /reservations/open.
	Route string `yaml:"route"`
	// Refusals are the refusals that the fill may return.
	Refusals []Refusal `yaml:"refusals,omitempty"`
	// Method is the protobuf method, as package.Service/Method, a unary method or a server stream, that serves the
	// query over gRPC.
	Method string `yaml:"method,omitempty"`
}
