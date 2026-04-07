# STEGO

Service Templates, Extensible Generation & Operations.

STEGO is a declarative code generator that eliminates accidental complexity
from service development. You describe what your service is in a YAML
declaration; STEGO deterministically generates production-ready code from
trusted, pre-built components.

## Why

LLMs are increasingly writing code. But code involves two kinds of
complexity: essential (your business logic) and accidental (framework
wiring, boilerplate, conventions). LLMs make different accidental choices
every time. Across 100 services, that means 100 subtly different
implementations of the same patterns. Unreviewable. Ungreppable. Unknown
risk surface.

STEGO removes accidental complexity mechanically. Trusted components make
every accidental decision once. The only thing left in the YAML and in
the fills is essential complexity.

## How it works

1. Pick an **archetype** (e.g. `rest-crud`), a curated set of components
   that determines your architecture, conventions, and defaults.
2. Write a **service declaration** (`service.yaml`), your entities,
   operations, and slot bindings. This is the only file the LLM produces.
3. Write **fills** for business logic, Go functions implementing typed
   slot contracts (protobuf). Tested and qualified by a human.
4. Run `stego apply` for deterministic code generation. Plain Go output.
   No runtime dependency on STEGO.

## Quick start

    stego init --archetype rest-crud --name my-service
    # edit service.yaml: add entities, expose operations
    stego plan          # see what will be generated
    stego apply         # generate code
    cd out && go build  # it's just Go

Add business logic:

    stego fill create admin-policy --slot before_create
    # implement fills/admin-policy/policy.go
    stego test          # run fill tests
    stego apply         # re-generate with fills wired in

## Concepts

Five nouns: **Archetype**, **Component**, **Mixin**, **Service Declaration**, **Fill**.
Seven operators: `use`, `with`, `mixin`, `gate`, `chain`, `fan-out`, `map`.

See [specs/spec.md](specs/spec.md) for the full specification.

## License

Apache License 2.0. See [LICENSE](LICENSE).
