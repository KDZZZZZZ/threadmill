# Crash-recovery acceptance fixture

This is a deliberately buggy, isolated acceptance project.

- Preserve every existing public Go API.
- Keep changes scoped to the requested subsystem in each branch.
- Do not delete, skip, weaken, or rewrite tests to manufacture a pass.
- Run the narrow package test in each independent branch.
- The integration owner must run `go test ./...` after all branches join.
