# Changelog

_Generated from release tags with `bash scripts/generate-changelog`._

## v0.7.1 (2026-07-16)

### Docs
- docs: clarify proxy listeners bind any address; document trust boundary (#46) (#50)

## v0.7.0 … v0.2.0 (2026-07-14)

### Features
- feat: web admin UI for viewing/editing listener config (#23) (#40)
- feat: add -validate flag to check config without binding listeners (#41)
- feat: validation-gated restarts + honest deploy failure (#42)
- feat: add -version flag baked via ldflags (#43)
- feat: add -log-level flag and CG_LOG_LEVEL env (#44)
- feat: uniform connection-lifecycle logging across the four TCP proxies (#45)

## v0.1.1 … v0.1.0 (2026-07-08)

### Features
- feat: release mechanism — auto-tag + conventional-commit CHANGELOG on push to main (#29)

### Fixes
- fix(release): commit CHANGELOG.md on first release (#34)

### Other Changes
- chore(release): re-trigger Release after dropped push event

