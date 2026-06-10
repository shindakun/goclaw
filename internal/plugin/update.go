package plugin

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Plugin update checks (phase 2 of docs/plugin-updates.md): detect that a newer release is
// available and let the operator update on demand, NEVER auto-applying. The check is a
// read-only `git ls-remote` of the upstream tags (no clone, no build, no sandbox needed: it
// reads remote ref names, like an HTTP GET). An actual update re-runs the FULL sandboxed Add
// at the newer ref, so updating an untrusted plugin goes through the exact same clone/scan/
// build/stage pipeline as the first install. The operator decides; goclaw only surfaces.

// parseSpec splits an install spec into its git URL, optional monorepo subdir, and optional
// pinned ref. The grammar is "<git-url>[#<subdir>][@<ref>]", e.g.
//
//	https://github.com/x/goclaw-roll
//	https://github.com/x/goclaw-gmail#cmd/gmail
//	https://github.com/x/goclaw-roll@v1.3.0
//	https://github.com/x/goclaw-gmail#cmd/gmail@v2.0.0
//
// The "@<ref>" is parsed off the END first (a ref cannot contain '@' or '#'), then "#<subdir>"
// off what remains, so a URL's own characters are never mistaken for a subdir/ref delimiter.
// Each part is validated; an invalid one is a hard error (fail closed).
func parseSpec(spec string) (gitURL, subdir, ref string, err error) {
	rest := strings.TrimSpace(spec)
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		ref = rest[i+1:]
		rest = rest[:i]
	}
	gitURL, subdir, _ = strings.Cut(rest, "#")
	if err := validateGitURL(gitURL); err != nil {
		return "", "", "", err
	}
	if err := validateSubdir(subdir); err != nil {
		return "", "", "", err
	}
	if err := validateRef(ref); err != nil {
		return "", "", "", err
	}
	return gitURL, subdir, ref, nil
}

// validateRef checks the optional "@<ref>" pin: a git tag, branch, or commit SHA. Empty is
// fine (the default branch). A non-empty ref must be a conservative git-refname-ish token:
// no whitespace, no shell metacharacters, no path traversal. It reaches the build container
// only as the PLUGIN_REF env var (never interpolated into the script), and git itself
// validates it as a real ref; this is the host-side first line.
func validateRef(ref string) error {
	if ref == "" {
		return nil
	}
	if strings.ContainsAny(ref, " \t\n;|&$`\\\"'*?~^:") {
		return fmt.Errorf("install: plugin ref contains illegal characters")
	}
	if strings.Contains(ref, "..") {
		return fmt.Errorf("install: plugin ref must not contain '..'")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("install: plugin ref must not start with '-'")
	}
	return nil
}

// UpdateStatus is the result of CheckUpdate for one plugin.
type UpdateStatus struct {
	Name           string // installed plugin name
	GitURL         string // upstream repo from provenance
	InstalledVer   string // installed plugin.yml version (bare semver)
	LatestTag      string // highest upstream v<semver> tag (e.g. "v1.4.0"), or "" if none
	UpdateAvail    bool   // a strictly-newer stable release tag exists upstream
	Provenanceless bool   // the plugin predates provenance tracking (no .source.json): cannot check
}

// CheckUpdate reads an installed plugin's provenance and compares its version against the
// highest stable v<semver> release tag upstream (via `git ls-remote`, read-only). It does NOT
// clone, build, or modify anything. A plugin with no provenance sidecar (installed before
// tracking) returns Provenanceless=true rather than an error, so a batch check can report it
// without failing.
func (in *Installer) CheckUpdate(ctx context.Context, name string) (*UpdateStatus, error) {
	clean := sanitizeName(name)
	if clean == "" {
		return nil, fmt.Errorf("install: invalid plugin name %q", name)
	}
	dir := filepath.Join(in.pluginsDir, clean)
	src, err := ReadSource(dir)
	if err != nil {
		// No sidecar (or unreadable): provenance unknown, cannot check upstream.
		return &UpdateStatus{Name: clean, Provenanceless: true}, nil
	}

	st := &UpdateStatus{Name: clean, GitURL: src.GitURL, InstalledVer: src.Version}

	tags, err := in.remoteTags(ctx, src.GitURL)
	if err != nil {
		return nil, fmt.Errorf("install: list upstream tags for %q: %w", clean, err)
	}
	latestTag, latestVer, ok := latestSemverTag(tags)
	if !ok {
		return st, nil // upstream ships no v<semver> release tags; nothing to compare
	}
	st.LatestTag = latestTag

	installedVer, perr := parseSemver(src.Version)
	if perr != nil {
		// The installed version is not parseable semver (a pre-provenance free-form version):
		// we can still SHOW the latest tag, but cannot assert "newer". Be conservative: only
		// flag an update when we can actually order the two.
		return st, nil
	}
	st.UpdateAvail = installedVer.less(latestVer)
	return st, nil
}

// remoteTags lists the tag names of a public repo without cloning it, using `git ls-remote
// --tags`. This is a read-only metadata fetch (no working tree, no build), safe to run on the
// host. Annotated-tag peel refs ("<tag>^{}") are normalised away so each tag appears once.
func (in *Installer) remoteTags(ctx context.Context, gitURL string) ([]string, error) {
	if err := validateGitURL(gitURL); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--tags", "--refs", gitURL)
	// No credential prompt: public repos only, fail fast on a private one.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, stderr strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var tags []string
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		// Each line: "<sha>\trefs/tags/<name>". --refs already drops the "^{}" peel lines.
		_, ref, found := strings.Cut(line, "\t")
		if !found {
			continue
		}
		tags = append(tags, strings.TrimPrefix(strings.TrimSpace(ref), "refs/tags/"))
	}
	return tags, nil
}

// Update re-installs a plugin at the newest upstream release tag, through the FULL sandboxed
// Add. It is the operator-triggered apply step: it checks for a newer tag, and if one exists,
// rebuilds from that tag (clone/scan/build/stage in the throwaway container, exactly like a
// fresh install) and atomically replaces the installed plugin. Returns the InstallResult and
// the status it acted on. If no newer release exists it makes NO change and returns a nil
// result with the status, so the caller can report "already up to date".
func (in *Installer) Update(ctx context.Context, name string) (*InstallResult, *UpdateStatus, error) {
	st, err := in.CheckUpdate(ctx, name)
	if err != nil {
		return nil, nil, err
	}
	if st.Provenanceless {
		return nil, st, fmt.Errorf("plugin %q has no provenance (installed before tracking); reinstall it with /plugin add <git-url> to enable updates", st.Name)
	}
	if !st.UpdateAvail {
		return nil, st, nil // already current; no rebuild
	}
	src, err := ReadSource(filepath.Join(in.pluginsDir, st.Name))
	if err != nil {
		return nil, st, fmt.Errorf("install: re-read provenance for %q: %w", st.Name, err)
	}
	// Rebuild from the newer tag through the unchanged sandboxed pipeline.
	spec := src.GitURL
	if src.Subdir != "" {
		spec += "#" + src.Subdir
	}
	spec += "@" + st.LatestTag
	res, err := in.Add(ctx, spec)
	if err != nil {
		return nil, st, err
	}
	return res, st, nil
}
