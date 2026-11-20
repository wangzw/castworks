package substrate

import (
	"context"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testAtespace     = "castworks-dev"
	testDigest       = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage        = "registry.zhanwei.wang/castworks/providertest@sha256:" + testDigest
	testControlPlane = "castworks-dev.castworks-system.svc.cluster.local:9090"
	testConnector    = "castworks-dev.castworks-system.svc.cluster.local:9091"
)

// expectedTemplate builds the template for the standard test configuration and
// returns it with its content-derived name.
func expectedTemplate(t *testing.T) *ateapi.ActorTemplate {
	t.Helper()
	tpl, err := buildActorTemplate(testAtespace, testImage, testControlPlane, testConnector)
	if err != nil {
		t.Fatalf("buildActorTemplate: %v", err)
	}
	return tpl
}

func expectedTemplateName(t *testing.T) string {
	t.Helper()
	return expectedTemplate(t).GetMetadata().GetName()
}

// templateClient is a ControlClient that stores at most one actor template.
// It records successful creations and creation attempts separately, because
// a create that loses a race returns ALREADY_EXISTS and is retried as a read.
type templateClient struct {
	ateapi.ControlClient

	mu        sync.Mutex
	template  *ateapi.ActorTemplate
	gets      int
	attempts  int
	created   int
	getErr    error
	createErr error
}

func (c *templateClient) GetActorTemplate(context.Context, *ateapi.GetActorTemplateRequest, ...grpc.CallOption) (*ateapi.ActorTemplate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.template == nil {
		return nil, status.Error(codes.NotFound, "actor template not found")
	}
	return c.template, nil
}

func (c *templateClient) CreateActorTemplate(_ context.Context, req *ateapi.CreateActorTemplateRequest, _ ...grpc.CallOption) (*ateapi.ActorTemplate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	if c.createErr != nil {
		return nil, c.createErr
	}
	if c.template != nil {
		return nil, status.Error(codes.AlreadyExists, "actor template already exists")
	}
	c.template = req.GetActorTemplate()
	c.created++
	return c.template, nil
}

func (c *templateClient) counts() (gets, attempts, created int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets, c.attempts, c.created
}

func TestActorTemplateName(t *testing.T) {
	build := func(controlPlane, connector string) *ateapi.ActorTemplate {
		t.Helper()
		tpl, err := buildActorTemplate(testAtespace, testImage, controlPlane, connector)
		if err != nil {
			t.Fatalf("buildActorTemplate: %v", err)
		}
		return tpl
	}

	base := build(testControlPlane, testConnector)
	name := base.GetMetadata().GetName()
	if !strings.HasPrefix(name, templateNamePrefix) {
		t.Fatalf("name = %q, want the %q prefix", name, templateNamePrefix)
	}
	if len(name) != len(templateNamePrefix)+12 {
		t.Fatalf("name = %q, want %q plus 12 hexadecimal digits", name, templateNamePrefix)
	}
	if _, err := hex.DecodeString(name[len(templateNamePrefix):]); err != nil {
		t.Fatalf("name %q does not end in hexadecimal digits: %v", name, err)
	}

	t.Run("same configuration", func(t *testing.T) {
		if got := build(testControlPlane, testConnector).GetMetadata().GetName(); got != name {
			t.Fatalf("name = %q, want %q", got, name)
		}
	})

	t.Run("repeated calculation", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			got, err := actorTemplateName(base)
			if err != nil {
				t.Fatalf("actorTemplateName: %v", err)
			}
			if got != name {
				t.Fatalf("call %d: name = %q, want %q", i+1, got, name)
			}
		}
	})

	t.Run("name field is excluded", func(t *testing.T) {
		renamed := build(testControlPlane, testConnector)
		renamed.Metadata.Name = "cw-ffffffffffff"
		got, err := actorTemplateName(renamed)
		if err != nil {
			t.Fatalf("actorTemplateName: %v", err)
		}
		if got != name {
			t.Fatalf("name = %q, want %q", got, name)
		}
	})

	t.Run("control plane changes the name", func(t *testing.T) {
		got := build("castworks-dev.castworks-system.svc.cluster.local:9099", testConnector).GetMetadata().GetName()
		if got == name {
			t.Fatalf("name = %q after changing the control plane, want a different name", got)
		}
	})

	t.Run("connector changes the name", func(t *testing.T) {
		got := build(testControlPlane, "castworks-dev.castworks-system.svc.cluster.local:9092").GetMetadata().GetName()
		if got == name {
			t.Fatalf("name = %q after changing the connector, want a different name", got)
		}
	})

	t.Run("image changes the name", func(t *testing.T) {
		tpl, err := buildActorTemplate(testAtespace, "registry.example/other@sha256:"+strings.Repeat("a", 64), testControlPlane, testConnector)
		if err != nil {
			t.Fatalf("buildActorTemplate: %v", err)
		}
		if got := tpl.GetMetadata().GetName(); got == name {
			t.Fatalf("name = %q after changing the image, want a different name", got)
		}
	})
}

// TestActorTemplateNameIgnoresMapOrder checks that the content digest does not
// depend on the order in which the worker selector's labels are inserted. The
// template carries eight labels, and the name is computed 25 times for the
// same labels in both forward and reverse insertion order; a map encoding that
// followed Go's randomized iteration order would almost surely disagree with
// the fixed name below at least once.
//
// want was computed with the recipe actorTemplateName documents, for exactly
// the labels in labelPairs: build the standard test template, replace its
// worker_selector.match_labels with those labels, clear metadata.name, marshal
// the template with proto.MarshalOptions{Deterministic: true}, take the SHA-256
// digest of those bytes, and use "cw-" plus the first 12 hexadecimal digits of
// the digest.
func TestActorTemplateNameIgnoresMapOrder(t *testing.T) {
	const want = "cw-cd26443166d6"

	labelPairs := []struct {
		key, value string
	}{
		{"castworks.dev/pool", "castworks"},
		{"castworks.dev/a", "1"},
		{"castworks.dev/b", "2"},
		{"castworks.dev/c", "3"},
		{"castworks.dev/d", "4"},
		{"castworks.dev/e", "5"},
		{"castworks.dev/f", "6"},
		{"castworks.dev/g", "7"},
	}
	buildWithLabels := func(reverse bool) string {
		t.Helper()
		tpl, err := buildActorTemplate(testAtespace, testImage, testControlPlane, testConnector)
		if err != nil {
			t.Fatalf("buildActorTemplate: %v", err)
		}
		labels := make(map[string]string, len(labelPairs))
		for i := range labelPairs {
			j := i
			if reverse {
				j = len(labelPairs) - 1 - i
			}
			labels[labelPairs[j].key] = labelPairs[j].value
		}
		tpl.WorkerSelector.MatchLabels = labels
		name, err := actorTemplateName(tpl)
		if err != nil {
			t.Fatalf("actorTemplateName: %v", err)
		}
		return name
	}

	for i := 0; i < 25; i++ {
		forward, reverse := buildWithLabels(false), buildWithLabels(true)
		if forward != reverse {
			t.Fatalf("round %d: names differ for the same labels inserted in a different order: %q != %q", i+1, forward, reverse)
		}
		if forward != want {
			t.Fatalf("round %d: name = %q, want the fixed %q", i+1, forward, want)
		}
	}
}

func TestEnsureActorTemplateCreates(t *testing.T) {
	wantName := expectedTemplateName(t)
	client := &templateClient{}
	name, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
	if err != nil {
		t.Fatalf("ensureActorTemplate: %v", err)
	}
	if name != wantName {
		t.Fatalf("template name = %q, want %q", name, wantName)
	}
	if gets, attempts, created := client.counts(); gets != 1 || attempts != 1 || created != 1 {
		t.Fatalf("gets/attempts/created = %d/%d/%d, want 1/1/1", gets, attempts, created)
	}

	tpl := client.template
	if got := tpl.GetMetadata().GetAtespace(); got != testAtespace {
		t.Errorf("atespace = %q, want %q", got, testAtespace)
	}
	if got := tpl.GetMetadata().GetName(); got != wantName {
		t.Errorf("name = %q, want %q", got, wantName)
	}
	if got := tpl.GetWorkerSelector().GetMatchLabels(); len(got) != 1 || got["castworks.dev/pool"] != "castworks" {
		t.Errorf("worker_selector.match_labels = %v, want castworks.dev/pool=castworks", got)
	}

	containers := tpl.GetContainers()
	if len(containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(containers))
	}
	container := containers[0]
	if got := container.GetName(); got != "warden" {
		t.Errorf("container name = %q, want warden", got)
	}
	if got := container.GetImage(); got != testImage {
		t.Errorf("container image = %q, want %q", got, testImage)
	}
	env := container.GetEnv()
	if len(env) != 2 {
		t.Fatalf("env = %d entries, want 2", len(env))
	}
	if got := env[0].GetName(); got != "CASTWORKS_CONTROL_PLANE" {
		t.Errorf("env[0] name = %q, want CASTWORKS_CONTROL_PLANE", got)
	}
	if got := env[0].GetValue(); got != testControlPlane {
		t.Errorf("CASTWORKS_CONTROL_PLANE = %q, want %q", got, testControlPlane)
	}
	if got := env[1].GetName(); got != "CASTWORKS_CONNECTOR" {
		t.Errorf("env[1] name = %q, want CASTWORKS_CONNECTOR", got)
	}
	if got := env[1].GetValue(); got != testConnector {
		t.Errorf("CASTWORKS_CONNECTOR = %q, want %q", got, testConnector)
	}
	probe := container.GetWakeupProbe()
	if got := probe.GetHttpGet().GetPort(); got != 8081 {
		t.Errorf("wakeup probe port = %d, want 8081", got)
	}
	if got := probe.GetHttpGet().GetPath(); got != "/readyz" {
		t.Errorf("wakeup probe path = %q, want /readyz", got)
	}
	if got := probe.GetTimeoutSeconds(); got != 120 {
		t.Errorf("wakeup probe timeout = %d, want 120", got)
	}
	mounts := container.GetVolumeMounts()
	if len(mounts) != 1 || mounts[0].GetName() != "system-info" || mounts[0].GetMountPath() != "/run/ate" {
		t.Errorf("volume_mounts = %v, want system-info at /run/ate", mounts)
	}

	volumes := tpl.GetVolumes()
	if len(volumes) != 1 {
		t.Fatalf("volumes = %d, want 1", len(volumes))
	}
	volume := volumes[0]
	if got := volume.GetName(); got != "system-info" {
		t.Errorf("volume name = %q, want system-info", got)
	}
	if volume.GetSystemInfo() == nil {
		t.Error("volume has no system_info source")
	}
	if !projectsActorName(volume.GetSystemInfo()) {
		t.Error("volume does not project the actor name to actor-name")
	}

	snapshotConfig := tpl.GetSnapshotConfig()
	if got := snapshotConfig.GetPreferredFidelity(); got != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		t.Errorf("preferred_fidelity = %v, want MEMORY", got)
	}
	if got := snapshotConfig.GetGoldenSnapshotConfig().GetMode(); got != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		t.Errorf("golden snapshot mode = %v, want DISABLED", got)
	}

	limits := tpl.GetResources().GetLimits()
	if got := limits.GetCpuMillicores(); got != 2000 {
		t.Errorf("cpu_millicores = %d, want 2000", got)
	}
	if got := limits.GetMemoryBytes(); got != 2<<30 {
		t.Errorf("memory_bytes = %d, want %d", got, 2<<30)
	}
}

func TestEnsureActorTemplateIsIdempotent(t *testing.T) {
	wantName := expectedTemplateName(t)
	client := &templateClient{}
	for i := 0; i < 2; i++ {
		name, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
		if err != nil {
			t.Fatalf("ensureActorTemplate call %d: %v", i+1, err)
		}
		if name != wantName {
			t.Fatalf("call %d: template name = %q, want %q", i+1, name, wantName)
		}
	}
	if _, attempts, created := client.counts(); attempts != 1 || created != 1 {
		t.Fatalf("attempts/created = %d/%d, want 1/1", attempts, created)
	}
}

func TestEnsureActorTemplateConcurrentCreate(t *testing.T) {
	wantName := expectedTemplateName(t)
	client := &templateClient{}
	const goroutines = 8
	var wg sync.WaitGroup
	names := make([]string, goroutines)
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], errs[i] = ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
		}(i)
	}
	wg.Wait()
	for i := range goroutines {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if names[i] != wantName {
			t.Fatalf("goroutine %d: name = %q, want %q", i, names[i], wantName)
		}
	}
	if _, _, created := client.counts(); created != 1 {
		t.Fatalf("successful creates = %d, want 1", created)
	}
}

func TestEnsureActorTemplateRejectsMismatch(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*ateapi.ActorTemplate)
	}{
		{
			name:  "image",
			field: "containers[warden].image",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].Image = "registry.example/other@sha256:" + strings.Repeat("a", 64)
			},
		},
		{
			name:  "control plane",
			field: "CASTWORKS_CONTROL_PLANE",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].Env[0].Value = "other:9090"
			},
		},
		{
			name:  "connector",
			field: "CASTWORKS_CONNECTOR",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].Env[1].Value = "other:9091"
			},
		},
		{
			name:  "missing connector",
			field: "CASTWORKS_CONNECTOR",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].Env = tpl.Containers[0].Env[:1]
			},
		},
		{
			name:  "probe path",
			field: "wakeup_probe.http_get.path",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].WakeupProbe.HttpGet.Path = "/healthz"
			},
		},
		{
			name:  "probe port",
			field: "wakeup_probe.http_get.port",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].WakeupProbe.HttpGet.Port = 8080
			},
		},
		{
			name:  "probe timeout",
			field: "wakeup_probe.timeout_seconds",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].WakeupProbe.TimeoutSeconds = 30
			},
		},
		{
			name:  "missing probe",
			field: "wakeup_probe",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].WakeupProbe = nil
			},
		},
		{
			name:  "missing container",
			field: "container \"warden\" is missing",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers = nil
			},
		},
		{
			name:  "mount path",
			field: "volume_mounts[system-info].mount_path",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Containers[0].VolumeMounts[0].MountPath = "/run/other"
			},
		},
		{
			name:  "missing volume",
			field: "volume \"system-info\" is missing",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Volumes = nil
			},
		},
		{
			name:  "volume type",
			field: "not a system_info volume",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Volumes[0].SystemInfo = nil
				tpl.Volumes[0].DurableDir = &ateapi.DurableDirVolumeSource{}
			},
		},
		{
			name:  "actor metadata path",
			field: "does not project ACTOR_METADATA_FIELD_NAME",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Volumes[0].SystemInfo.DataSources[0].ActorMetadata.Items[0].Path = "name"
			},
		},
		{
			name:  "preferred fidelity",
			field: "snapshot_config.preferred_fidelity",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.SnapshotConfig.PreferredFidelity = ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS
			},
		},
		{
			name:  "golden snapshot mode",
			field: "snapshot_config.golden_snapshot_config.mode",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.SnapshotConfig.GoldenSnapshotConfig.Mode = ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED
			},
		},
		{
			name:  "worker selector",
			field: "worker_selector.match_labels",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.WorkerSelector.MatchLabels["castworks.dev/pool"] = "other"
			},
		},
		{
			name:  "cpu",
			field: "resources.limits.cpu_millicores",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Resources.Limits.CpuMillicores = 1000
			},
		},
		{
			name:  "memory",
			field: "resources.limits.memory_bytes",
			mutate: func(tpl *ateapi.ActorTemplate) {
				tpl.Resources.Limits.MemoryBytes = 1 << 30
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tpl := expectedTemplate(t)
			tt.mutate(tpl)
			client := &templateClient{template: tpl}

			_, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
			if err == nil {
				t.Fatal("ensureActorTemplate succeeded, want a mismatch error")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("error %q does not name the field %q", err, tt.field)
			}
			if _, attempts, created := client.counts(); attempts != 0 || created != 0 {
				t.Fatalf("attempts/created = %d/%d, want 0/0", attempts, created)
			}
		})
	}
}

func TestEnsureActorTemplateErrors(t *testing.T) {
	t.Run("get fails", func(t *testing.T) {
		client := &templateClient{getErr: status.Error(codes.Internal, "boom")}
		_, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
		if err == nil || status.Code(err) != codes.Internal {
			t.Fatalf("err = %v, want code Internal", err)
		}
	})

	t.Run("create fails", func(t *testing.T) {
		client := &templateClient{createErr: status.Error(codes.InvalidArgument, "bad template")}
		_, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
		if err == nil || status.Code(err) != codes.InvalidArgument {
			t.Fatalf("err = %v, want code InvalidArgument", err)
		}
	})
}

func TestEnsureActorTemplateReadsBackAfterAlreadyExists(t *testing.T) {
	tpl := expectedTemplate(t)
	client := &appearingTemplateClient{templateClient: &templateClient{}, tpl: tpl}
	name, err := ensureActorTemplate(context.Background(), client, testAtespace, testImage, testControlPlane, testConnector)
	if err != nil {
		t.Fatalf("ensureActorTemplate: %v", err)
	}
	if want := tpl.GetMetadata().GetName(); name != want {
		t.Fatalf("name = %q, want %q", name, want)
	}
	if _, attempts, created := client.counts(); attempts != 1 || created != 0 {
		t.Fatalf("attempts/created = %d/%d, want 1/0", attempts, created)
	}
}

// appearingTemplateClient makes the template appear between the first get and
// the create, so the create loses the race with a concurrent creator.
type appearingTemplateClient struct {
	*templateClient
	tpl      *ateapi.ActorTemplate
	appeared bool
}

func (c *appearingTemplateClient) GetActorTemplate(context.Context, *ateapi.GetActorTemplateRequest, ...grpc.CallOption) (*ateapi.ActorTemplate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.appeared {
		return c.template, nil
	}
	return nil, status.Error(codes.NotFound, "actor template not found")
}

func (c *appearingTemplateClient) CreateActorTemplate(context.Context, *ateapi.CreateActorTemplateRequest, ...grpc.CallOption) (*ateapi.ActorTemplate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts++
	c.template = c.tpl
	c.appeared = true
	return nil, status.Error(codes.AlreadyExists, "actor template already exists")
}
