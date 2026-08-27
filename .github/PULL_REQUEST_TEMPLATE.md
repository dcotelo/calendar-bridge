# Summary

<!-- What does this PR do, and why? -->

## Test plan

- [ ] `go build ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race`
- [ ] `gofmt -l .` clean
- [ ] Manually verified against real Google accounts (if this touches `internal/googleauth` or `internal/sync`)

## Checklist

- [ ] Tests added/updated for any behavior change
- [ ] README updated if user-facing behavior, config, or setup steps changed
- [ ] No secrets, tokens, or credentials committed (check `git diff` before pushing)
