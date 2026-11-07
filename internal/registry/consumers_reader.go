package registry

// ConsumersReader holds the component consumers value that one run built in
// memory from the committed records of the checkouts it was given, and hands
// it to the query handlers. Nothing is kept between runs.
type ConsumersReader struct {
	uses []ServiceUse
}

// Uses gives every recorded use of a component, by component and service.
func (r *ConsumersReader) Uses() []ServiceUse {
	if r == nil {
		return nil
	}
	return r.uses
}
