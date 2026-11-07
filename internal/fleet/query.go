package fleet

// Hit is one service that pins the digest asked for, with the component and the version it holds.
type Hit struct {
	Service string `json:"service"`
	Path    string `json:"path"`
	Pin
}

// Answer is the answer to Find every service built from a component digest: each checkout once, as pinning the
// digest, as not pinning it, or as unknown.
type Answer struct {
	Digest     string    `json:"digest"`
	Pinning    []Hit     `json:"pinning"`
	NotPinning []Service `json:"not_pinning"`
	Unknown    []Unknown `json:"unknown"`
	Checkouts  int       `json:"checkouts"`
}

// FindByDigest is the query Find every service built from a component digest (ST-DIS-QRY01): every recorded
// service in the sweep that pins the digest, with the version and digest it holds, every other recorded service,
// and every checkout without a readable record as unknown. It reads the sweep through the reader alone.
func FindByDigest(reader *SweepReader, digest string) Answer {
	sweep := reader.Sweep()
	answer := Answer{Digest: digest, Unknown: append([]Unknown(nil), sweep.Unknown...), Checkouts: len(sweep.Order)}
	for _, service := range sweep.Services {
		pinned := false
		for _, pin := range service.Pins {
			if pin.Digest == digest {
				answer.Pinning = append(answer.Pinning, Hit{Service: service.Name, Path: service.Path, Pin: pin})
				pinned = true
			}
		}
		if !pinned {
			answer.NotPinning = append(answer.NotPinning, service)
		}
	}
	return answer
}
