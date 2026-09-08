package compiler

func hasFallibleConstructor(input AssemblerInput, consumed map[constructorKey]bool) bool {
	for key, active := range consumed {
		if active && input.Wirings[key.WiringIndex].Wiring.ConstructorReturnsError[key.ConstructorIndex] {
			return true
		}
	}
	return false
}
