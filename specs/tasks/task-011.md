# Task 011: jwt-auth Component Generator

**Spec Reference:** "MVP Scope", "Port Resolution"

**Status:** `complete`

## Description

Implement the `jwt-auth` component as a Go package with a `Generator`.

- `registry/components/jwt-auth/component.yaml`
- Generator produces:
  - JWT middleware for HTTP handlers
  - Token validation logic
  - Identity extraction (populates `stego.common.Identity`)
  - Configurable header (default `Authorization`, overridable via service.yaml)
- Provides: `auth-provider`
- Output namespace: `internal/auth`

## Spec Excerpt

> `jwt-auth` component (MVP Scope)
> Service-level override example: `jwt-auth: { header: X-Internal-Token }`

## Acceptance Criteria

- Generator produces compilable Go auth middleware
- Header is configurable
- Identity struct populated from JWT claims
- Tests verify generated code compiles

## Task Completion

When done, update this file's Status to `complete` and list relevant commits below.

## Commits

- 936e683 feat(task-011): implement jwt-auth component generator
