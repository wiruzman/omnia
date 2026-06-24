# Building
After every code change, add tests if necessary, run tests, and state which project or projects need to be rebuilt. Build only the affected executable(s) unless a shared change requires rebuilding both.

# Release
When asked to release, follow `docs/release.md` as the source of truth before taking release actions. Use the automated GitHub Actions release path by default, and use the manual steps only as a fallback when automation is not appropriate.
