# Dependency command environment

The dependency runner must resolve the source that STEGO captured. A saved
Go source overlay must not replace those inputs during `stego deps`.

The prior runner set `GOFLAGS` to an empty value. Go then reads that setting
from its configuration file. See the [Go configuration implementation](https://raw.githubusercontent.com/golang/go/go1.26.8/src/cmd/go/internal/cfg/cfg.go).
A saved `-overlay` flag could therefore change the effective imports without
changing the files in STEGO's before-and-after snapshots.

[Regression run 35500594177](https://github.com/jsell-rh/stego/actions/runs/35500594177)
proved this with the unchanged dependency runner from signed compiler c515f22.
Resolution succeeded, but the module gained a dependency from source outside
the snapshot. The test then failed on that incorrect result. The private proxy
control and all other selected dependency tests passed. The
[regression record](dependency-environment-regression-evidence.json) contains
source and result hashes. This failing run does not qualify a compiler.

The fix sets `GOFLAGS` to a single space. This non-empty value takes precedence
over saved flags. Go parses it as no flags. See the [Go flag parser](https://raw.githubusercontent.com/golang/go/go1.26.8/src/cmd/go/internal/base/goflags.go).
`GOWORK=off` and the explicit staged module file remain. Saved private proxy and
module settings remain available. The fix does not disable the Go configuration
file or change the user's saved settings.

A real Go regression test supplies both a saved overlay and an invalid ambient
flag. A separate local HTTP fixture checks that saved proxy selection still
works. Existing dependency transaction, recovery, lock, and source-change
checks remain. The fix requires focused and full hosted qualification.

This corrects one input-control gap. It does not establish complete toolchain,
application build environment, or dependency-source identity. C3 remains open.
No new Hypershell compiler pin is part of this change; its current workflow
continues with the fixed c515f22 candidate.
