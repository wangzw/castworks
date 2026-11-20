// The actor template this file creates follows
// docs/milestone/v0.1.0/sandboxprovider.md "actor 模板" and the v0.2.0
// additions in docs/milestone/v0.2.0/sandboxprovider.md "actor 模板". It is
// created through ate-api, never updated and never deleted: Substrate has no
// update RPC for templates, and deleting a template that actors still
// reference breaks their later resumes. The template creation is adapted from
// Substrate v1.0.0's cmd/ate-setup/internal/steps/substrate.go and is used
// under the Apache License, Version 2.0.

package substrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Fixed values of the actor template. They are not configurable through
// Config: the pool, the actor size and the probe are the same for every actor.
const (
	templateNamePrefix = "cw-"

	templateContainerName = "warden"
	templateControlPlane  = "CASTWORKS_CONTROL_PLANE"
	templateConnector     = "CASTWORKS_CONNECTOR"
	templateVolumeName    = "system-info"
	templateVolumePath    = "/run/ate"

	templateProbePath           = "/readyz"
	templateProbePort           = int32(8081)
	templateProbeTimeoutSeconds = int32(120)

	templatePoolLabelKey   = "castworks.dev/pool"
	templatePoolLabelValue = "castworks"

	templateCPUMillicores = int64(2000)
	templateMemoryBytes   = int64(2 << 30) // 2 GiB
)

// actorTemplateName derives the template name from the template content:
// "cw-" followed by the first 12 hexadecimal digits of the SHA-256 digest of
// the deterministic protobuf encoding of the template with its name cleared.
// The image digest is part of the content, and so are the control-plane and
// connector addresses; a configuration change therefore yields a new name
// instead of a mismatch against an immutable existing template.
func actorTemplateName(tpl *ateapi.ActorTemplate) (string, error) {
	if tpl.GetMetadata() == nil {
		return "", errors.New("actor template has no metadata")
	}
	name := tpl.GetMetadata().GetName()
	tpl.GetMetadata().Name = ""
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(tpl)
	tpl.GetMetadata().Name = name
	if err != nil {
		return "", fmt.Errorf("marshal actor template for its content digest: %w", err)
	}
	digest := sha256.Sum256(data)
	return templateNamePrefix + hex.EncodeToString(digest[:])[:12], nil
}

// ensureActorTemplate returns the name of the actor template for the given
// image and addresses, creating the template when it does not exist. An
// existing template is never modified or deleted; instead its key fields are
// checked against the values this provider would create, and a mismatch is an
// error. A create that loses a race with another process (ALREADY_EXISTS) is
// followed by a read of the winning template.
func ensureActorTemplate(ctx context.Context, client ateapi.ControlClient, atespace, image, controlPlane, connector string) (string, error) {
	tpl, err := buildActorTemplate(atespace, image, controlPlane, connector)
	if err != nil {
		return "", err
	}
	name := tpl.GetMetadata().GetName()
	ref := &ateapi.ObjectRef{Atespace: atespace, Name: name}

	tpl, err = client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref})
	if err == nil {
		if err := checkActorTemplate(tpl, image, controlPlane, connector); err != nil {
			return "", err
		}
		return name, nil
	}
	if status.Code(err) != codes.NotFound {
		return "", fmt.Errorf("get actor template %s/%s: %w", atespace, name, err)
	}

	tpl, err = buildActorTemplate(atespace, image, controlPlane, connector)
	if err != nil {
		return "", err
	}
	if _, err := client.CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: tpl}); err != nil {
		if status.Code(err) != codes.AlreadyExists {
			return "", fmt.Errorf("create actor template %s/%s: %w", atespace, name, err)
		}
		// Another process created the template between our get and create.
		tpl, err = client.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return "", fmt.Errorf("get actor template %s/%s after a concurrent create: %w", atespace, name, err)
		}
		if err := checkActorTemplate(tpl, image, controlPlane, connector); err != nil {
			return "", err
		}
		return name, nil
	}

	slog.Default().Info("created actor template", "atespace", atespace, "template", name, "image", image)
	return name, nil
}

// buildActorTemplate returns the template for image, controlPlane and
// connector, with its name derived from the content. Its fields are the tables
// in sandboxprovider.md "actor 模板"; checkActorTemplate verifies the ones
// that would break a running provider if an existing template disagreed.
func buildActorTemplate(atespace, image, controlPlane, connector string) (*ateapi.ActorTemplate, error) {
	tpl := &ateapi.ActorTemplate{
		Metadata: &ateapi.ResourceMetadata{Atespace: atespace},
		WorkerSelector: &ateapi.Selector{MatchLabels: map[string]string{
			templatePoolLabelKey: templatePoolLabelValue,
		}},
		Containers: []*ateapi.Container{{
			Name:  templateContainerName,
			Image: image,
			Env: []*ateapi.EnvVar{{
				Name:  templateControlPlane,
				Value: controlPlane,
			}, {
				Name:  templateConnector,
				Value: connector,
			}},
			WakeupProbe: &ateapi.WakeupProbe{
				HttpGet: &ateapi.HttpGetAction{
					Path: templateProbePath,
					Port: templateProbePort,
				},
				TimeoutSeconds: templateProbeTimeoutSeconds,
			},
			VolumeMounts: []*ateapi.VolumeMount{{
				Name:      templateVolumeName,
				MountPath: templateVolumePath,
			}},
		}},
		Volumes: []*ateapi.Volume{{
			Name: templateVolumeName,
			SystemInfo: &ateapi.SystemInfoVolumeSource{
				DataSources: []*ateapi.SystemInfoDataSource{{
					ActorMetadata: &ateapi.ActorMetadataDataSource{
						Items: []*ateapi.ActorMetadataItem{{
							Field: ateapi.ActorMetadataField_ACTOR_METADATA_FIELD_NAME,
							Path:  "actor-name",
						}},
					},
				}},
			},
		}},
		SnapshotConfig: &ateapi.SnapshotConfig{
			PreferredFidelity: ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			GoldenSnapshotConfig: &ateapi.GoldenSnapshotConfig{
				Mode: ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED,
			},
		},
		Resources: &ateapi.ResourceRequirements{
			Limits: &ateapi.Resources{
				CpuMillicores: templateCPUMillicores,
				MemoryBytes:   templateMemoryBytes,
			},
		},
	}
	name, err := actorTemplateName(tpl)
	if err != nil {
		return nil, err
	}
	tpl.Metadata.Name = name
	return tpl, nil
}

// checkActorTemplate compares the key fields of an existing template with the
// values buildActorTemplate would create. Every mismatch is reported with the
// field it was found in.
func checkActorTemplate(tpl *ateapi.ActorTemplate, image, controlPlane, connector string) error {
	var errs []error
	name := tpl.GetMetadata().GetName()
	field := func(path string, want, got any) {
		errs = append(errs, fmt.Errorf("actor template %s: %s = %v, want %v", name, path, got, want))
	}

	var container *ateapi.Container
	for _, c := range tpl.GetContainers() {
		if c.GetName() == templateContainerName {
			container = c
			break
		}
	}
	if container == nil {
		errs = append(errs, fmt.Errorf("actor template %s: container %q is missing", name, templateContainerName))
	} else {
		if container.GetImage() != image {
			field("containers["+templateContainerName+"].image", image, container.GetImage())
		}

		for _, want := range []struct {
			name  string
			value string
		}{
			{templateControlPlane, controlPlane},
			{templateConnector, connector},
		} {
			value, found := "", false
			for _, env := range container.GetEnv() {
				if env.GetName() == want.name {
					value, found = env.GetValue(), true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Errorf("actor template %s: environment variable %s is missing", name, want.name))
			} else if value != want.value {
				field("containers["+templateContainerName+"].env["+want.name+"]", want.value, value)
			}
		}

		probe := container.GetWakeupProbe()
		if probe == nil {
			errs = append(errs, fmt.Errorf("actor template %s: containers[%s].wakeup_probe is missing", name, templateContainerName))
		} else {
			if got := probe.GetHttpGet().GetPath(); got != templateProbePath {
				field("containers["+templateContainerName+"].wakeup_probe.http_get.path", templateProbePath, got)
			}
			if got := probe.GetHttpGet().GetPort(); got != templateProbePort {
				field("containers["+templateContainerName+"].wakeup_probe.http_get.port", templateProbePort, got)
			}
			if got := probe.GetTimeoutSeconds(); got != templateProbeTimeoutSeconds {
				field("containers["+templateContainerName+"].wakeup_probe.timeout_seconds", templateProbeTimeoutSeconds, got)
			}
		}

		mountFound := false
		for _, mount := range container.GetVolumeMounts() {
			if mount.GetName() != templateVolumeName {
				continue
			}
			mountFound = true
			if got := mount.GetMountPath(); got != templateVolumePath {
				field("containers["+templateContainerName+"].volume_mounts["+templateVolumeName+"].mount_path", templateVolumePath, got)
			}
		}
		if !mountFound {
			errs = append(errs, fmt.Errorf("actor template %s: containers[%s].volume_mounts has no %q mount", name, templateContainerName, templateVolumeName))
		}
	}

	var volume *ateapi.Volume
	for _, v := range tpl.GetVolumes() {
		if v.GetName() == templateVolumeName {
			volume = v
			break
		}
	}
	if volume == nil {
		errs = append(errs, fmt.Errorf("actor template %s: volume %q is missing", name, templateVolumeName))
	} else if volume.GetSystemInfo() == nil {
		errs = append(errs, fmt.Errorf("actor template %s: volume %q is not a system_info volume", name, templateVolumeName))
	} else if !projectsActorName(volume.GetSystemInfo()) {
		errs = append(errs, fmt.Errorf("actor template %s: volume %q does not project ACTOR_METADATA_FIELD_NAME to actor-name", name, templateVolumeName))
	}

	snapshotConfig := tpl.GetSnapshotConfig()
	if got := snapshotConfig.GetPreferredFidelity(); got != ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY {
		field("snapshot_config.preferred_fidelity", ateapi.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY, got)
	}
	if got := snapshotConfig.GetGoldenSnapshotConfig().GetMode(); got != ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED {
		field("snapshot_config.golden_snapshot_config.mode", ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED, got)
	}

	matchLabels := tpl.GetWorkerSelector().GetMatchLabels()
	if len(matchLabels) != 1 || matchLabels[templatePoolLabelKey] != templatePoolLabelValue {
		field("worker_selector.match_labels", map[string]string{templatePoolLabelKey: templatePoolLabelValue}, matchLabels)
	}

	limits := tpl.GetResources().GetLimits()
	if got := limits.GetCpuMillicores(); got != templateCPUMillicores {
		field("resources.limits.cpu_millicores", templateCPUMillicores, got)
	}
	if got := limits.GetMemoryBytes(); got != templateMemoryBytes {
		field("resources.limits.memory_bytes", templateMemoryBytes, got)
	}

	return errors.Join(errs...)
}

// projectsActorName reports whether the system_info volume projects the
// actor name to the path the warden reads.
func projectsActorName(source *ateapi.SystemInfoVolumeSource) bool {
	for _, ds := range source.GetDataSources() {
		for _, item := range ds.GetActorMetadata().GetItems() {
			if item.GetField() == ateapi.ActorMetadataField_ACTOR_METADATA_FIELD_NAME && item.GetPath() == "actor-name" {
				return true
			}
		}
	}
	return false
}
