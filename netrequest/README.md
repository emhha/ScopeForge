# Netloom integration

ScopeForge selects the native EHole and Wappalyzer matchers with
`fingerprint.NewBuiltin(ctx, fingerprint.SourceEHole, fingerprint.SourceWappalyzer)`.
Netloom owns the bundled rules and compiles the matchers. ScopeForge neither
copies rules nor locates, extracts, or configures a rules directory.

Rule file paths in netloom's `fingerprint.Config` are only needed when callers
supply external rules. The HTTP passive integration requires no rule paths.
