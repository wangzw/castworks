//go:build acceptance

// Snapshot size measurement for the acceptance tests.
//
// Substrate's provider records Timing.SnapshotBytes as 0 because ate-api v1.0.0
// does not expose a snapshot's size. The real size is measured here, from the
// objects rustfs wrote to its local PV, before the actor is ended: whether the
// objects survive DeleteActor is unknown and this code does not rely on it.
//
// # URI to directory mapping
//
// ate-api builds an actor snapshot URI in internal/resources/snapshot.go as
//
//	<location>/atespaces/<atespace>/actors/<actor UID>/snapshots/<snapshot name>
//
// where <location> is the ActorTemplate's snapshotConfig.storage_location, or
// the cluster-wide --snapshot-location. On this cluster the location is
// gs://ate-snapshots/, so a URI looks like
//
//	gs://ate-snapshots/atespaces/castworks-dev/actors/<uid>/snapshots/<name>
//
// Measured on the development host (2026-11-19), the PV ate-system-rustfs is
// bound to the host path
//
//	/data2/k8s-local-pv/ate-system/rustfs/
//
// (not /data2/k8s-local-pv/ate-system-rustfs/, the name the acceptance plan
// uses for the PV; /data2/k8s-local-pv/<name>/ is the convention, and this PV's
// directory is nested one level deeper). Under that root the layout is
//
//	<PV root>/ate-snapshots/<object key>
//
// with the object key mirroring the URI path. rustfs stores every object as a
// directory of one or more part.N data files plus xl.meta metadata:
//
//	ate-snapshots/atespaces/<atespace>/actors/<uid>/snapshots/<name>/
//	    xl.meta
//	    <uuid>/part.1 ...
//
// so the bytes and files under the snapshot's directory are the snapshot's
// local footprint. A layout that cannot be mapped to the URI is reported as
// unmeasured with the reason; it is never reported as zero bytes.

package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	// defaultRustFSPVDir is where the ate-system-rustfs PV is mounted on the
	// development host. See the package comment for how it was measured.
	defaultRustFSPVDir = "/data2/k8s-local-pv/ate-system/rustfs"

	// snapshotBucket is the bucket the cluster-wide snapshot location names.
	snapshotBucket = "ate-snapshots"

	// kubeContextEnv carries the optional Kubernetes context for kubectl, set
	// by the acceptance Makefile targets from KUBE_CONTEXT.
	kubeContextEnv = "CASTWORKS_ACC_KUBE_CONTEXT"
)

// snapshotCommandRunner runs one read-only command and returns its standard
// output, standard error and error. It exists so the measurement can be tested
// without a cluster or a real PV.
type snapshotCommandRunner interface {
	run(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)
}

// execSnapshotRunner runs commands on the development host.
type execSnapshotRunner struct{}

func (execSnapshotRunner) run(ctx context.Context, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// snapshotMeasurement is one snapshot's size. Measured is false when the
// snapshot could not be measured: Bytes is then omitted rather than set to 0,
// and Error says why.
type snapshotMeasurement struct {
	URI      string `json:"snapshot_uri"`
	Measured bool   `json:"measured"`
	Bytes    int64  `json:"bytes,omitempty"`
	Files    int    `json:"files,omitempty"`
	Error    string `json:"error,omitempty"`
}

// snapshotSizeSummary summarizes the measured snapshots. Unmeasured snapshots
// are counted separately and are not part of the median, p95 or maximum.
type snapshotSizeSummary struct {
	Count       int   `json:"count"`
	Unmeasured  int   `json:"unmeasured"`
	MedianBytes int64 `json:"median_bytes"`
	P95Bytes    int64 `json:"p95_bytes"`
	MaxBytes    int64 `json:"max_bytes"`
}

// snapshotSizer measures snapshots of one actor from the rustfs local PV.
type snapshotSizer struct {
	pvRoot      string
	kubectl     string
	kubeContext string
	run         snapshotCommandRunner
}

// newDefaultSnapshotSizer returns the sizer the acceptance run uses: kubectl on
// PATH, the context from CASTWORKS_ACC_KUBE_CONTEXT when set, and the rustfs PV
// mounted on the development host.
func newDefaultSnapshotSizer() snapshotSizer {
	return snapshotSizer{
		pvRoot:      defaultRustFSPVDir,
		kubectl:     "kubectl",
		kubeContext: os.Getenv(kubeContextEnv),
		run:         execSnapshotRunner{},
	}
}

// measureActor reads every snapshot URI of actor in atespace and measures each
// one. It must be called before the actor is ended. A snapshot that cannot be
// measured is returned with Measured false and Error set; only a failure to
// read or parse the actor fails the whole call.
func (s snapshotSizer) measureActor(ctx context.Context, atespace, actor string) ([]snapshotMeasurement, error) {
	if atespace == "" || actor == "" {
		return nil, fmt.Errorf("snapshot size: atespace and actor must not be empty")
	}
	args := make([]string, 0, 9)
	if s.kubeContext != "" {
		args = append(args, "--context", s.kubeContext)
	}
	args = append(args, "ate", "get", "actor", actor, "-a", atespace, "-o", "json")

	stdout, stderr, err := s.run.run(ctx, s.kubectl, args...)
	if err != nil {
		return nil, snapshotCommandError(fmt.Sprintf("%s %s", s.kubectl, strings.Join(args, " ")), stderr, err)
	}
	uris, err := actorSnapshotURIs([]byte(stdout))
	if err != nil {
		return nil, fmt.Errorf("snapshot size: parsing actor %s/%s: %w", atespace, actor, err)
	}
	if len(uris) == 0 {
		return nil, fmt.Errorf("snapshot size: actor %s/%s has no status.snapshots", atespace, actor)
	}
	measurements := make([]snapshotMeasurement, 0, len(uris))
	for i, uri := range uris {
		if uri == "" {
			// An entry without a URI is kept as unmeasured, with the reason,
			// instead of being dropped from the count.
			measurements = append(measurements, snapshotMeasurement{
				Error: fmt.Sprintf("status.snapshots[%d] has no snapshot URI", i),
			})
			continue
		}
		measurements = append(measurements, s.measureURI(ctx, uri))
	}
	return measurements, nil
}

// measureURI measures one snapshot URI. Failures are recorded on the returned
// measurement, not returned as an error, so one unreadable prefix does not hide
// the sizes of the other snapshots.
func (s snapshotSizer) measureURI(ctx context.Context, uri string) snapshotMeasurement {
	m := snapshotMeasurement{URI: uri}
	dir, err := snapshotURIToLocalDir(s.pvRoot, uri)
	if err != nil {
		m.Error = err.Error()
		return m
	}
	bytes, files, err := snapshotDirSize(ctx, s.run, dir)
	if err != nil {
		m.Error = err.Error()
		return m
	}
	m.Measured = true
	m.Bytes = bytes
	m.Files = files
	return m
}

// snapshotURIToLocalDir maps a snapshot URI to the directory holding its
// objects under the rustfs local PV root. It accepts only actor snapshots under
// the ate-snapshots bucket and the layout described in the package comment.
func snapshotURIToLocalDir(pvRoot, uri string) (string, error) {
	if pvRoot == "" {
		return "", fmt.Errorf("snapshot size: empty PV root")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("snapshot size: invalid snapshot URI %q: %w", uri, err)
	}
	if u.Scheme != "gs" {
		return "", fmt.Errorf("snapshot size: snapshot URI %q has scheme %q, want gs", uri, u.Scheme)
	}
	if u.Host != snapshotBucket {
		return "", fmt.Errorf("snapshot size: snapshot URI %q is in bucket %q, want %q", uri, u.Host, snapshotBucket)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("snapshot size: snapshot URI %q has userinfo, query or fragment", uri)
	}

	key := strings.Trim(u.Path, "/")
	segments := strings.Split(key, "/")
	// <root>/atespaces/<atespace>/actors/<uid>/snapshots/<name>; the last
	// six segments are fixed by resources.NewActorSnapshotURI. Checking the
	// suffix rather than the last "atespaces" segment keeps a snapshot whose
	// name is literally "atespaces" from shifting the match.
	idx := len(segments) - 6
	if idx < 0 || segments[idx] != "atespaces" || segments[idx+2] != "actors" || segments[idx+4] != "snapshots" {
		return "", fmt.Errorf("snapshot size: snapshot URI %q does not end in atespaces/<atespace>/actors/<uid>/snapshots/<name>", uri)
	}
	atespace, uid, name := segments[idx+1], segments[idx+3], segments[idx+5]
	for _, segment := range []string{atespace, uid, name} {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, `/\`) {
			return "", fmt.Errorf("snapshot size: snapshot URI %q has an invalid path segment %q", uri, segment)
		}
	}
	// The joined path must stay under pvRoot/ate-snapshots/. A ".." before the
	// atespaces suffix would otherwise be cleaned away by filepath.Join and
	// move the measurement outside the bucket.
	base := filepath.Join(pvRoot, snapshotBucket)
	dir := filepath.Join(base, filepath.FromSlash(key))
	if !strings.HasPrefix(dir, base+string(filepath.Separator)) {
		return "", fmt.Errorf("snapshot size: snapshot URI %q maps to %q, outside %q", uri, dir, base)
	}
	return dir, nil
}

// actorSnapshotURIs extracts the snapshot URIs from the output of
// `kubectl ate get actor -o json`. kubectl prints the ate-api proto with
// protojson, so the durable snapshot is
//
//	status.snapshots[].durableSnapshot.object.snapshotUri
//
// (camelCase JSON names). The acceptance plan names the same field as
// status.snapshots[].storage.object.snapshot_uri. The walk below accepts either
// spelling and looks anywhere inside each snapshot entry, so a change in the
// wrapper message does not silently produce an empty measurement.
//
// The result has one element per status.snapshots[] entry. An entry without a
// snapshot URI yields the empty string: the caller records it as unmeasured
// with the reason instead of dropping it, so the snapshot count stays honest.
func actorSnapshotURIs(actorJSON []byte) ([]string, error) {
	var doc struct {
		Status struct {
			Snapshots []json.RawMessage `json:"snapshots"`
		} `json:"status"`
	}
	if err := json.Unmarshal(actorJSON, &doc); err != nil {
		return nil, fmt.Errorf("decoding actor JSON: %w", err)
	}
	uris := make([]string, 0, len(doc.Status.Snapshots))
	for i, raw := range doc.Status.Snapshots {
		var snapshot any
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			return nil, fmt.Errorf("decoding status.snapshots[%d]: %w", i, err)
		}
		uris = append(uris, findSnapshotURI(snapshot))
	}
	return uris, nil
}

// findSnapshotURI returns the first non-empty snapshot_uri or snapshotUri value
// in value, walking maps and slices in a deterministic order.
func findSnapshotURI(value any) string {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key != "snapshot_uri" && key != "snapshotUri" {
				continue
			}
			if uri, ok := v[key].(string); ok && uri != "" {
				return uri
			}
		}
		for _, key := range keys {
			if uri := findSnapshotURI(v[key]); uri != "" {
				return uri
			}
		}
	case []any:
		for _, item := range v {
			if uri := findSnapshotURI(item); uri != "" {
				return uri
			}
		}
	}
	return ""
}

// snapshotDirSize returns the bytes and file count under dir, using
// `sudo -n du -sb` and `sudo -n find`. Both commands only read; -n makes sudo
// fail immediately rather than wait for a password when passwordless sudo is
// not configured. A missing directory or a prefix with no files is an error: an
// empty measurement must never be reported as zero bytes.
func snapshotDirSize(ctx context.Context, run snapshotCommandRunner, dir string) (int64, int, error) {
	stdout, stderr, err := run.run(ctx, "sudo", "-n", "du", "-sb", dir)
	if err != nil {
		return 0, 0, snapshotCommandError(fmt.Sprintf("sudo -n du -sb %s", dir), stderr, err)
	}
	firstLine := strings.TrimSpace(stdout)
	if newline := strings.IndexByte(firstLine, '\n'); newline >= 0 {
		firstLine = firstLine[:newline]
	}
	fields := strings.Fields(firstLine)
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("snapshot size: unexpected sudo -n du -sb %s output %q", dir, stdout)
	}
	bytes, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("snapshot size: parsing sudo -n du -sb %s output %q: %w", dir, fields[0], err)
	}

	stdout, stderr, err = run.run(ctx, "sudo", "-n", "find", dir, "-type", "f", "-printf", ".")
	if err != nil {
		return 0, 0, snapshotCommandError(fmt.Sprintf("sudo -n find %s -type f", dir), stderr, err)
	}
	files := len(stdout)
	if files == 0 {
		return 0, 0, fmt.Errorf("snapshot size: no objects under %s", dir)
	}
	return bytes, files, nil
}

// summarizeSnapshotSizes summarizes the successfully measured snapshots.
// Median is the middle value, or the average of the two middle values for an
// even count. P95 uses the nearest-rank method: the value at
// ceil(0.95 * count), one-indexed.
func summarizeSnapshotSizes(measurements []snapshotMeasurement) snapshotSizeSummary {
	var summary snapshotSizeSummary
	values := make([]int64, 0, len(measurements))
	for _, m := range measurements {
		if !m.Measured {
			summary.Unmeasured++
			continue
		}
		values = append(values, m.Bytes)
	}
	summary.Count = len(values)
	if len(values) == 0 {
		return summary
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	summary.MaxBytes = values[len(values)-1]
	if len(values)%2 == 1 {
		summary.MedianBytes = values[len(values)/2]
	} else {
		summary.MedianBytes = (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	rank := (95*len(values) + 99) / 100 // ceil(0.95 * n), one-indexed
	summary.P95Bytes = values[rank-1]
	return summary
}

// snapshotCommandError includes the command's standard error in the error.
func snapshotCommandError(description, stderr string, err error) error {
	if stderr = strings.TrimSpace(stderr); stderr != "" {
		return fmt.Errorf("%s: %w: %s", description, err, stderr)
	}
	return fmt.Errorf("%s: %w", description, err)
}

// fakeSnapshotRunner records the commands it is given and answers them with
// fn. It lets the unit tests exercise the measurement without sudo, kubectl or
// a working PV.
type fakeSnapshotRunner struct {
	calls []string
	fn    func(name string, args []string) (stdout, stderr string, err error)
}

func (f *fakeSnapshotRunner) run(_ context.Context, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if f.fn == nil {
		return "", "", fmt.Errorf("unexpected command %s %v", name, args)
	}
	return f.fn(name, args)
}

// TestSnapshotSizeUnit covers the parts that need neither a cluster nor the
// rustfs PV: URI mapping, actor JSON extraction, command parsing and the
// summary.
func TestSnapshotSizeUnit(t *testing.T) {
	const validURI = "gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/name-1"

	t.Run("DefaultSizer", func(t *testing.T) {
		t.Setenv(kubeContextEnv, "ctx-1")
		s := newDefaultSnapshotSizer()
		if s.pvRoot != defaultRustFSPVDir {
			t.Errorf("pvRoot = %q, want %q", s.pvRoot, defaultRustFSPVDir)
		}
		if s.kubectl != "kubectl" {
			t.Errorf("kubectl = %q, want kubectl", s.kubectl)
		}
		if s.kubeContext != "ctx-1" {
			t.Errorf("kubeContext = %q, want ctx-1", s.kubeContext)
		}
		if s.run == nil {
			t.Error("run is nil")
		}
	})

	t.Run("URIMapping", func(t *testing.T) {
		const root = "/pv"
		tests := []struct {
			name    string
			uri     string
			want    string
			wantErr string
		}{
			{
				name: "plain",
				uri:  validURI,
				want: filepath.Join(root, "ate-snapshots", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "name-1"),
			},
			{
				name: "trailing slash",
				uri:  validURI + "/",
				want: filepath.Join(root, "ate-snapshots", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "name-1"),
			},
			{
				name: "location with a root prefix",
				uri:  "gs://ate-snapshots/prefix/atespaces/castworks-dev/actors/uid-1/snapshots/name-1",
				want: filepath.Join(root, "ate-snapshots", "prefix", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "name-1"),
			},
			{
				name: "snapshot named atespaces",
				uri:  "gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/atespaces",
				want: filepath.Join(root, "ate-snapshots", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "atespaces"),
			},
			{name: "wrong scheme", uri: "s3://ate-snapshots/atespaces/a/actors/u/snapshots/n", wantErr: "want gs"},
			{name: "wrong bucket", uri: "gs://other/atespaces/a/actors/u/snapshots/n", wantErr: "want \"ate-snapshots\""},
			{name: "bare bucket", uri: "gs://ate-snapshots/", wantErr: "does not end in"},
			{name: "tag snapshot", uri: "gs://ate-snapshots/atespaces/a/tags/u", wantErr: "does not end in"},
			{name: "missing snapshot name", uri: "gs://ate-snapshots/atespaces/a/actors/u/snapshots", wantErr: "does not end in"},
			{name: "dot segment", uri: "gs://ate-snapshots/atespaces/a/actors/../snapshots/n", wantErr: "invalid path segment"},
			{
				// A ".." in the prefix is cleaned away by filepath.Join and
				// would move the measurement outside the snapshot bucket.
				name:    "dot-dot escapes the bucket",
				uri:     "gs://ate-snapshots/../evil/atespaces/a/actors/u/snapshots/n",
				wantErr: "outside",
			},
			{name: "query", uri: validURI + "?x=1", wantErr: "query or fragment"},
			{name: "invalid URI", uri: "://bad", wantErr: "invalid snapshot URI"},
			{name: "empty root", uri: validURI, wantErr: "empty PV root"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				pvRoot := root
				if tt.name == "empty root" {
					pvRoot = ""
				}
				got, err := snapshotURIToLocalDir(pvRoot, tt.uri)
				if tt.wantErr != "" {
					if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
						t.Fatalf("snapshotURIToLocalDir(%q) error = %v, want containing %q", tt.uri, err, tt.wantErr)
					}
					return
				}
				if err != nil {
					t.Fatalf("snapshotURIToLocalDir(%q): %v", tt.uri, err)
				}
				if got != tt.want {
					t.Errorf("snapshotURIToLocalDir(%q) = %q, want %q", tt.uri, got, tt.want)
				}
			})
		}
	})

	t.Run("ActorJSON", func(t *testing.T) {
		protojsonShape := []byte(`{
			"status": {
				"snapshots": [
					{"uuid": "u1", "durableSnapshot": {"state": "SNAPSHOT_STORAGE_STATE_COMPLETED", "object": {"snapshotUri": "gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/s1"}}},
					{"uuid": "u2", "durableSnapshot": {"state": "SNAPSHOT_STORAGE_STATE_PENDING", "object": {"snapshotUri": "gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/s2"}}},
					{"uuid": "u3"}
				]
			}
		}`)
		uris, err := actorSnapshotURIs(protojsonShape)
		if err != nil {
			t.Fatalf("actorSnapshotURIs: %v", err)
		}
		want := []string{
			"gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/s1",
			"gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/s2",
			// u3 has no snapshot URI; it is kept as an empty entry so the
			// caller records it as unmeasured instead of dropping it.
			"",
		}
		if len(uris) != len(want) {
			t.Fatalf("actorSnapshotURIs = %v, want %v", uris, want)
		}
		for i := range want {
			if uris[i] != want[i] {
				t.Errorf("actorSnapshotURIs[%d] = %q, want %q", i, uris[i], want[i])
			}
		}

		planShape := []byte(`{
			"status": {
				"snapshots": [
					{"uuid": "u1", "storage": {"object": {"snapshot_uri": "gs://ate-snapshots/atespaces/castworks-dev/actors/uid-1/snapshots/s1"}}}
				]
			}
		}`)
		uris, err = actorSnapshotURIs(planShape)
		if err != nil {
			t.Fatalf("actorSnapshotURIs plan shape: %v", err)
		}
		if len(uris) != 1 || uris[0] != want[0] {
			t.Errorf("actorSnapshotURIs plan shape = %v, want [%s]", uris, want[0])
		}

		if _, err := actorSnapshotURIs([]byte("not json")); err == nil {
			t.Error("actorSnapshotURIs(invalid JSON) returned no error")
		}
		uris, err = actorSnapshotURIs([]byte(`{"status": {"snapshots": [{"uuid": "u1"}]}}`))
		if err != nil {
			t.Fatalf("actorSnapshotURIs(no URI): %v", err)
		}
		if len(uris) != 1 || uris[0] != "" {
			t.Errorf("actorSnapshotURIs(no URI) = %v, want one empty entry", uris)
		}
	})

	t.Run("DirectorySize", func(t *testing.T) {
		const dir = "/pv/ate-snapshots/atespaces/a/actors/u/snapshots/n"
		run := &fakeSnapshotRunner{fn: func(name string, args []string) (string, string, error) {
			switch {
			case name == "sudo" && args[0] == "-n" && args[1] == "du":
				return "12345\t" + dir + "\n", "", nil
			case name == "sudo" && args[0] == "-n" && args[1] == "find":
				return "...", "", nil
			default:
				return "", "", fmt.Errorf("unexpected command %s %v", name, args)
			}
		}}
		bytes, files, err := snapshotDirSize(context.Background(), run, dir)
		if err != nil {
			t.Fatalf("snapshotDirSize: %v", err)
		}
		if bytes != 12345 || files != 3 {
			t.Errorf("snapshotDirSize = (%d, %d), want (12345, 3)", bytes, files)
		}
		wantCalls := []string{
			"sudo -n du -sb " + dir,
			"sudo -n find " + dir + " -type f -printf .",
		}
		if len(run.calls) != len(wantCalls) {
			t.Fatalf("calls = %v, want %v", run.calls, wantCalls)
		}
		for i := range wantCalls {
			if run.calls[i] != wantCalls[i] {
				t.Errorf("call %d = %q, want %q", i, run.calls[i], wantCalls[i])
			}
		}

		t.Run("missing directory", func(t *testing.T) {
			run := &fakeSnapshotRunner{fn: func(string, []string) (string, string, error) {
				return "", "du: cannot access: No such file or directory", fmt.Errorf("exit status 1")
			}}
			_, _, err := snapshotDirSize(context.Background(), run, dir)
			if err == nil || !strings.Contains(err.Error(), "No such file or directory") {
				t.Fatalf("snapshotDirSize error = %v, want the du error", err)
			}
		})

		t.Run("no files", func(t *testing.T) {
			run := &fakeSnapshotRunner{fn: func(name string, args []string) (string, string, error) {
				if args[1] == "du" {
					return "0\t" + dir + "\n", "", nil
				}
				return "", "", nil
			}}
			_, _, err := snapshotDirSize(context.Background(), run, dir)
			if err == nil || !strings.Contains(err.Error(), "no objects") {
				t.Fatalf("snapshotDirSize error = %v, want no objects", err)
			}
		})

		t.Run("password required", func(t *testing.T) {
			run := &fakeSnapshotRunner{fn: func(string, []string) (string, string, error) {
				return "", "sudo: a password is required", fmt.Errorf("exit status 1")
			}}
			_, _, err := snapshotDirSize(context.Background(), run, dir)
			if err == nil || !strings.Contains(err.Error(), "a password is required") {
				t.Fatalf("snapshotDirSize error = %v, want the sudo -n error", err)
			}
			if len(run.calls) != 1 || !strings.Contains(run.calls[0], "-n") {
				t.Errorf("calls = %v, want one sudo -n command", run.calls)
			}
		})

		t.Run("bad du output", func(t *testing.T) {
			run := &fakeSnapshotRunner{fn: func(string, []string) (string, string, error) {
				return "garbage\n", "", nil
			}}
			_, _, err := snapshotDirSize(context.Background(), run, dir)
			if err == nil || !strings.Contains(err.Error(), "unexpected sudo -n du") {
				t.Fatalf("snapshotDirSize error = %v, want unexpected output", err)
			}
		})
	})

	t.Run("MeasureURI", func(t *testing.T) {
		dir := filepath.Join(defaultRustFSPVDir, "ate-snapshots", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "name-1")
		run := &fakeSnapshotRunner{fn: func(name string, args []string) (string, string, error) {
			if args[1] == "du" {
				return "9876\t" + dir + "\n", "", nil
			}
			return "...", "", nil
		}}
		s := snapshotSizer{pvRoot: defaultRustFSPVDir, run: run}
		m := s.measureURI(context.Background(), validURI)
		if !m.Measured || m.Error != "" {
			t.Fatalf("measureURI = %+v, want measured without error", m)
		}
		if m.Bytes != 9876 || m.Files != 3 {
			t.Errorf("measureURI = (%d, %d), want (9876, 3)", m.Bytes, m.Files)
		}

		m = s.measureURI(context.Background(), "gs://ate-snapshots/atespaces/a/tags/u")
		if m.Measured || m.Error == "" {
			t.Errorf("measureURI(tag URI) = %+v, want unmeasured with an error", m)
		}
	})

	t.Run("MeasureActor", func(t *testing.T) {
		dir := filepath.Join("/pv", "ate-snapshots", "atespaces", "castworks-dev", "actors", "uid-1", "snapshots", "name-1")
		actorJSON := `{"status": {"snapshots": [{"durableSnapshot": {"object": {"snapshotUri": "` + validURI + `"}}}]}}`
		run := &fakeSnapshotRunner{fn: func(name string, args []string) (string, string, error) {
			switch {
			case name == "kubectl":
				return actorJSON, "", nil
			case args[1] == "du":
				return "42\t" + dir + "\n", "", nil
			default:
				return "...", "", nil
			}
		}}
		s := snapshotSizer{pvRoot: "/pv", kubectl: "kubectl", kubeContext: "ctx-1", run: run}
		measurements, err := s.measureActor(context.Background(), "castworks-dev", "cw-run-1")
		if err != nil {
			t.Fatalf("measureActor: %v", err)
		}
		if len(measurements) != 1 || !measurements[0].Measured || measurements[0].Bytes != 42 {
			t.Fatalf("measureActor = %+v, want one measured snapshot of 42 bytes", measurements)
		}
		wantKubectl := "kubectl --context ctx-1 ate get actor cw-run-1 -a castworks-dev -o json"
		if len(run.calls) == 0 || run.calls[0] != wantKubectl {
			t.Errorf("first call = %v, want %q", run.calls, wantKubectl)
		}

		// An entry without a URI must surface as unmeasured, not vanish.
		partialJSON := `{"status": {"snapshots": [
			{"durableSnapshot": {"object": {"snapshotUri": "` + validURI + `"}}},
			{"uuid": "u2"}
		]}}`
		partialRun := &fakeSnapshotRunner{fn: func(name string, args []string) (string, string, error) {
			switch {
			case name == "kubectl":
				return partialJSON, "", nil
			case args[1] == "du":
				return "42\t" + dir + "\n", "", nil
			default:
				return "...", "", nil
			}
		}}
		s = snapshotSizer{pvRoot: "/pv", kubectl: "kubectl", run: partialRun}
		measurements, err = s.measureActor(context.Background(), "castworks-dev", "cw-run-1")
		if err != nil {
			t.Fatalf("measureActor(missing URI): %v", err)
		}
		if len(measurements) != 2 {
			t.Fatalf("measureActor(missing URI) = %+v, want two entries", measurements)
		}
		if !measurements[0].Measured || measurements[0].Bytes != 42 {
			t.Errorf("measurements[0] = %+v, want measured with 42 bytes", measurements[0])
		}
		if measurements[1].Measured || !strings.Contains(measurements[1].Error, "no snapshot URI") {
			t.Errorf("measurements[1] = %+v, want unmeasured with a missing-URI reason", measurements[1])
		}
		if summary := summarizeSnapshotSizes(measurements); summary.Count != 1 || summary.Unmeasured != 1 {
			t.Errorf("summary = %+v, want count 1 and unmeasured 1", summary)
		}

		allMissing := &fakeSnapshotRunner{fn: func(string, []string) (string, string, error) {
			return `{"status": {"snapshots": [{"uuid": "u1"}, {"uuid": "u2"}]}}`, "", nil
		}}
		s = snapshotSizer{pvRoot: "/pv", kubectl: "kubectl", run: allMissing}
		measurements, err = s.measureActor(context.Background(), "castworks-dev", "cw-run-1")
		if err != nil {
			t.Fatalf("measureActor(all URIs missing): %v", err)
		}
		if len(measurements) != 2 || measurements[0].Measured || measurements[1].Measured {
			t.Errorf("measureActor(all URIs missing) = %+v, want two unmeasured entries", measurements)
		}

		emptyRun := &fakeSnapshotRunner{fn: func(string, []string) (string, string, error) {
			return `{"status": {"snapshots": []}}`, "", nil
		}}
		s = snapshotSizer{pvRoot: "/pv", kubectl: "kubectl", run: emptyRun}
		if _, err := s.measureActor(context.Background(), "castworks-dev", "cw-run-1"); err == nil {
			t.Error("measureActor(no snapshots) returned no error")
		}
	})

	t.Run("Summary", func(t *testing.T) {
		measurements := []snapshotMeasurement{
			{URI: "a", Measured: true, Bytes: 10},
			{URI: "b", Measured: true, Bytes: 20},
			{URI: "c", Measured: true, Bytes: 30},
			{URI: "d", Measured: true, Bytes: 40},
			{URI: "e", Measured: true, Bytes: 100},
			{URI: "f", Measured: false, Error: "unmeasurable"},
		}
		got := summarizeSnapshotSizes(measurements)
		want := snapshotSizeSummary{Count: 5, Unmeasured: 1, MedianBytes: 30, P95Bytes: 100, MaxBytes: 100}
		if got != want {
			t.Errorf("summarizeSnapshotSizes = %+v, want %+v", got, want)
		}

		got = summarizeSnapshotSizes([]snapshotMeasurement{{Measured: true, Bytes: 10}, {Measured: true, Bytes: 20}})
		if got.MedianBytes != 15 || got.P95Bytes != 20 {
			t.Errorf("even-count summary = %+v, want median 15 and p95 20", got)
		}

		values := make([]snapshotMeasurement, 20)
		for i := range values {
			values[i] = snapshotMeasurement{Measured: true, Bytes: int64(i + 1)}
		}
		got = summarizeSnapshotSizes(values)
		if got.P95Bytes != 19 {
			t.Errorf("p95 of 1..20 = %d, want 19 (nearest rank)", got.P95Bytes)
		}

		if got := summarizeSnapshotSizes(nil); got != (snapshotSizeSummary{}) {
			t.Errorf("summarizeSnapshotSizes(nil) = %+v, want zero", got)
		}
	})
}
