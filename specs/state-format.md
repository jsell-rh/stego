# Compiler state format

STEGO writes `format_version: 1` in `.stego/state.yaml`. The version identifies
the outer state schema. The input manifest and recovery journal retain their
separate versions.

The reader accepts version 1 and legacy version 0. An absent version means
legacy version 0. An explicit version must be a YAML integer. Negative values,
unsupported versions, and unknown fields fail validation. No output changes
can start from an unsupported state record. State in a recovery journal has
the same validation requirement.

Reading legacy state does not change the file. Planning records the upgrade as
a state change. The next apply saves version 1 through the normal recovery
journal, after output changes. A state-only upgrade does not rewrite generated
files. Repeated apply has no further format change. The upgrade preserves the
existing component, entity, file, input, and compiler records.

Recovery completes the exact bytes in a valid saved journal. A journal from an
older compiler can therefore complete with legacy state. A later plan records
the version upgrade. Recovery does not silently change the saved transaction.
An unsupported future state version leaves the journal and output unchanged.

Older compilers that do not know `format_version` reject the new field through
strict decoding. Downgrade is not automatic. To return to an older compiler,
restore its reviewed project inputs, generated output, and state together from
version control. Do not delete state or remove its version to force acceptance.
Resolve a pending transaction with a compatible compiler before a downgrade.

Future changes that require different state fields or meanings must use a new
format version and define the supported read and upgrade paths. A reader must
reject versions that it does not implement. It must not discard unknown fields
or infer compatibility from the compiler revision string.

Focused tests cover legacy reads, saved-record preservation, unsupported
versions before writes, interrupted state-only upgrades, exact legacy journal
recovery, and future journal rejection. All six jobs in
[CI run 35175826484](https://github.com/jsell-rh/stego/actions/runs/35175826484)
passed at `288d60b`, including the full race suite and both example projects.
The compiler package passed in 112.593 seconds and the command package in
72.292 seconds. See the [source and result record](state-format-evidence.json).
This format does not establish compiler artifact trust, full build input
identity, or power-loss recovery.
