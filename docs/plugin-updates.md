# Plugin update checks (notify, never auto-apply)

Status: DESIGN / RFC. No code. How goclaw should tell the operator that an installed
plugin has a newer version available, WITHOUT ever updating it automatically. Plugins are
untrusted code the operator chose to install; a silent auto-update would re-run arbitrary
untrusted code on the operator's behalf, exactly what the install sandbox + operator-in-the-
loop model exists to prevent. So the goal is: **detect and surface; the operator decides.**

Read `internal/plugin/install.go` (the sandboxed clone/scan/build/stage pipeline) and
`docs/security.md` first.

## 1. The hard prerequisite: goclaw does not currently remember where a plugin came from

An update check needs to compare "what is installed" against "what is upstream." goclaw can
do the first half but not the second, because it throws the provenance away:

- `Installer.acceptArtifact` COMPUTES the source commit (`/out/.commit`) and the version,
  and returns them in `InstallResult`. But it only copies the BINARY and `plugin.yml` into
  the installed dir (`data/plugins/<name>/`). The git URL, the subdir, and the installed
  commit are NOT persisted anywhere.
- So at rest, an installed plugin is just `{binary, plugin.yml}`. goclaw cannot answer "what
  repo/commit is this from?", which is the input an update check requires.

**Therefore step one of any option below is: persist provenance per installed plugin.** A
small record written at install time:

```
git_url       the source repo (e.g. https://github.com/shindakun/goclaw-gmail)
subdir        the monorepo subdir, or "" (e.g. cmd/gmail)
commit        the exact commit the installed binary was built from
version       plugin.yml `version` at install time
installed_at  timestamp
```

**DECISION: a sidecar file (`data/plugins/<name>/.source.json`), not a DB table.** The
record is dot-prefixed so the runner's watch ignores it, and the installer writes it into
the atomic staging dir (`.<name>.installing`) before the rename, so it lands atomically with
the binary + plugin.yml. Rationale:

- **Single source of truth.** What is installed already lives in the filesystem
  (`data/plugins/<name>/`), and the poll-reconcile loop treats that dir as authoritative.
  Provenance is a PROPERTY of an installed plugin, so it belongs in the same place. A DB
  table would create a SECOND source of truth that must be kept in sync with the dir, and
  this codebase has already been bitten by two-stores-drift bugs (the inotify-vs-filesystem
  gap, the delivery ledger). Do not reintroduce that shape for ~5 plugins.
- **It travels with the plugin.** Remove the dir and the provenance goes with it: no orphan
  rows, no cleanup step, no "DB says installed but the dir is gone" skew.
- **Tamper-safe enough.** The plugin dir is host-owned and mounted READ-ONLY into the
  container, so the container cannot rewrite `.source.json` to spoof its version, the same
  protection a DB would give, without the DB.
- **The DB's only real advantage is history**, and history is a separate concern handled by
  logging (below), not a reason to make the live provenance record relational. The query
  advantage ("SELECT ... WHERE update_available") is irrelevant at goclaw's plugin count.

Revisit ONLY if provenance ever becomes cross-plugin and relational (a dependency graph, a
shared registry cache); a SQL query over a handful of files is not that signal.

This provenance record is independently useful (audit: "what is installed, from where, at
what commit"), so it is worth doing even before the update check.

### Install/remove logging (currently ABSENT, add it alongside provenance)

Today goclaw does NOT log or persist installs at all. `pluginAdd`/`pluginRemove`
(`internal/router/router.go`) only return a transient CHAT reply to the owner ("Installed
gmail v1.0.0 at abc12345"); the git URL and commit are echoed and then DISCARDED (not even
written into the plugin dir). So an install leaves no durable trace once the chat scrolls.

Add logging when we touch the install path for provenance, two cheap, additive pieces:

- A structured host log line on every add/remove: `log.Info("plugin installed", "name",
  ..., "git_url", ..., "subdir", ..., "commit", ..., "version", ...)` and the matching
  `"plugin removed"`. Zero new storage; shows up in the host console/journal.
- A durable audit HISTORY of add/remove events. This is the separate "history" concern the
  sidecar deliberately does NOT cover (the sidecar holds only CURRENT state and is overwritten
  on update). It was briefly its own `data/plugins/.install-log.jsonl`, but that parallel log
  is now FOLDED INTO the host operational event log (`internal/eventlog`, kinds `plugin.install`
  / `plugin.remove`), so there is one operational history store, not two. Keeping history as an
  append-only LOG, not as DB rows, preserves the single-source-of-truth property: the sidecar
  is current state, the event log is the event trail, neither is a queryable mutable store that
  can drift from the filesystem.

So: sidecar for current provenance, a log line (and optional jsonl) for history. Do both
when implementing phase 1, since the install path is already being edited to write the
sidecar.

## 2. What "an update is available" can MEAN (two signals)

There is no single "is there an update" question; it depends on what upstream exposes. We use
the two DELIBERATE signals (an author had to do something to publish them) and reject the
implicit one.

### 2a. Manifest version change (the fallback)

Compare the installed `plugin.yml` `version` against the `version` in the upstream
`plugin.yml` at the checked ref (fetch just that one file, or shallow-clone). If the author
bumped `version`, that is a deliberate "this is a new release" signal. Cost: the author must
actually bump `version` on meaningful changes (a discipline; goclawkit's handoff proposes a
CI lint to enforce it). This reuses the manifest field that already exists, and is the
fallback for a plugin that is not tag-released.

### 2b. Release tags (strongest, the primary signal)

Compare the installed version against the latest semver RELEASE TAG on the repo
(`git ls-remote --tags <url>`, pick the highest semver for this plugin). A tag like `v1.2.0`
is an explicit, deliberate, immutable "this is a released version" marker, the clearest
possible signal, and it pins what an update would install (a tag, not a moving branch). This
is the GitHub-release / semver-tag model. It asks the most of plugin authors (cut a tagged
release) but gives the cleanest operator experience and the safest update target.

### Rejected: commit drift

Comparing the installed commit against the tip of the default branch (`git ls-remote <url>
HEAD`) was considered and REJECTED. It needs no author cooperation, but it is too noisy to be
useful: every README typo or unrelated commit reads as "update available," and for a monorepo
(`#subdir`) most commits do not even touch the plugin's subdir. We rely on the deliberate
signals (2a, 2b) instead; an author who wants their plugin to advertise updates publishes a
version bump or a tag.

**The "should we require releases + version tags?" question (your prompt):** yes, lean
toward REQUIRING a semver tag for a plugin to participate in update checks, but degrade
gracefully:

- A plugin installed from a tag (`<url>@v1.2.0`, a tag spec we would add) gets proper
  tag-based update checks (2b): "v1.2.0 installed, v1.3.0 available." The convention is one
  plugin per repo, so a bare `v<semver>` tag is unambiguous (see goclawkit
  `docs/sdk-spec.md`, "Releasing a plugin").
- A plugin installed from a bare URL / branch still works, but its update check falls back to
  2a (version-in-manifest), clearly labeled as less precise. There is NO commit-drift
  fallback: a plugin whose author publishes neither a tag nor a version bump simply reports
  "no update signal" rather than crying wolf on every upstream commit.

So tags are the BLESSED path (and what the docs should steer authors toward), not a hard gate
that breaks the existing bare-URL installs. This also means the install spec grows an
optional `@<tag>` (or `@<commit>`) pin, and "update" becomes "re-install at the newer tag,"
which is reproducible.

## 3. When to check, and how the operator is told

### When

- **On demand:** a `goclaw plugin check` / `goclaw plugin outdated` command the operator
  runs. Always available, zero background cost, no surprise network calls. This is the
  minimum and the v0.
- **Periodic, opt-in:** a background check every N hours that records results; OFF by
  default (a network call to every plugin's upstream is a privacy/footprint choice the
  operator should opt into). When on, it never installs, only updates the "available"
  state.

### How surfaced (NEVER auto-applied)

- `goclaw plugin list` annotates each plugin: `gmail  v1.2.0  (v1.3.0 available)`.
- `goclaw plugin check` prints only the ones with updates, plus the exact command to update
  each: `goclaw plugin update gmail` (which re-runs the sandboxed install at the newer
  tag/commit, same pipeline, same scan, operator-initiated).
- Optionally, if a maintenance/owner channel is configured, a once-a-day "N plugins have
  updates" line to the owner (reusing the maintenance-summary delivery path), informational,
  with the update commands. Opt-in, rate-limited, never noisy.

The cardinal rule, stated in the doc and the command help: **goclaw never installs or
updates a plugin without an explicit operator action.** A detected update changes a
displayed STATE, nothing else. Updating is always a fresh trip through the install sandbox
(clone, scan for red flags, build, stage), because the new version is new untrusted code and
must be re-vetted exactly like a first install.

## 4. Security considerations specific to updates

- **An update is untrusted code, re-vetted from scratch.** `plugin update` is not a fast
  path; it is a full sandboxed re-install of the new ref. The red-flag scan, the
  host-secret-read scan, the build-in-a-throwaway-container, all run again. A plugin that was
  safe at v1.2.0 can be hostile at v1.3.0; the operator is consenting to the NEW code.
- **Pin what you check and what you install.** Checking against a moving branch then
  installing "latest" is a TOCTOU-ish window. Tag-based (2b) closes it: you are told
  "v1.3.0 available," and `update` installs exactly v1.3.0, the thing you were told about.
- **Provenance integrity.** The `.source.json` sidecar lives in the host-owned plugin dir
  (mounted read-only into the container), so the container cannot rewrite it to spoof its own
  version. Keep it host-side; never trust a version the container reports about itself for
  the update decision (read the manifest the HOST staged, not one the plugin emits).
- **No silent network.** Update checks make outbound calls to plugin source repos. On-demand
  is explicit. Periodic must be opt-in and should be visible (logged), so the operator is not
  surprised by goclaw reaching out to GitHub on a timer.

## 5. Proposed phasing

1. **Provenance + logging (foundational):** persist `{git_url, subdir, commit, version,
   installed_at}` per plugin at install time as a sidecar `.source.json`, and add the
   install/remove history (now recorded as `plugin.*` events in `internal/eventlog`).
   Surface provenance in `goclaw plugin list`. Independently useful as an audit record. NO
   update-checking logic yet.
2. **On-demand check:** `goclaw plugin check` / `outdated` using the strongest signal the
   provenance supports (tag > manifest-version; no commit-drift fallback), printing the
   update command.
   Add the optional `@<tag>`/`@<commit>` pin to the install spec and a `goclaw plugin update
   <name>` that re-installs at the newer ref through the full sandbox.
3. **Author guidance (tags as the blessed path):** document that plugins SHOULD ship semver
   release tags; tagged installs get precise update checks, bare-URL installs get the weaker
   fallback. This is where goclawkit needs a handoff (section 6).
4. **Optional periodic check + owner notification:** opt-in background check that updates the
   "available" state and (if a channel is configured) sends a daily informational summary.
   Off by default.

## 6. What this asks of goclawkit (handoff to write later)

goclaw owns the MECHANISM (provenance, checking, the operator surface). goclawkit is where
plugin AUTHORS live, so it owns the CONVENTION authors follow. A later goclawkit handoff
should cover:

- **Versioning discipline:** `plugin.yml` `version` MUST be semver and MUST be bumped on any
  behavior change (today it is free-form and often left at `1.0.0`; gmail-tools shipped while
  the channel was `1.0.0`). The handshake `Info.Version` must agree.
- **Release tags as the blessed distribution:** authors cut a semver git tag (`v<semver>`)
  per release. SETTLED: one plugin per repo, so a bare `v<semver>` tag is unambiguous, no
  per-plugin tag namespacing is needed. (`goclaw-gmail` currently ships two plugins from one
  repo; it is to be split into one-plugin repos so this convention holds.)
- **A changelog/`CHANGELOG.md` convention** (optional) so `goclaw plugin check` can show WHAT
  changed, not just that something did.
- Possibly an SDK helper or a `goclawkit`-side lint that fails CI if `version` was not bumped
  when code changed, to make the discipline enforceable rather than aspirational.

These conventions are now documented in goclawkit `docs/sdk-spec.md` ("Releasing a plugin"):
semver `version`, bare `v<semver>` release tags, the `@<ref>` install pin, and the
`CHANGELOG.md` convention. The tag-namespacing question is resolved (one plugin per repo),
so tag-based checks (2b) can be the primary signal.

## 7. Recommendation in one line

Persist provenance now (phase 1, useful on its own), then an on-demand `plugin check` that
prefers release tags and falls back to manifest version, never auto-applying; push authors
toward semver tags via goclawkit. Tag namespacing is settled (one plugin per repo, bare
`v<semver>`), so tags can be the primary signal.
