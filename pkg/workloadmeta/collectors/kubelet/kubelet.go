// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// +build kubelet

package kubelet

import (
	"context"
	"errors"
	"time"

	"k8s.io/kubernetes/third_party/forked/golang/expansion"

	"github.com/DataDog/datadog-agent/pkg/config"
	"github.com/DataDog/datadog-agent/pkg/util/containers"
	"github.com/DataDog/datadog-agent/pkg/util/kubernetes/kubelet"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/datadog-agent/pkg/workloadmeta"
)

const (
	collectorID = "kubelet"
	expireFreq  = 15 * time.Second
)

type collector struct {
	watcher    *kubelet.PodWatcher
	store      *workloadmeta.Store
	lastExpire time.Time
	expireFreq time.Duration
}

func init() {
	workloadmeta.RegisterCollector(collectorID, func() workloadmeta.Collector {
		return &collector{}
	})
}

func (c *collector) Start(_ context.Context, store *workloadmeta.Store) error {
	if !config.IsFeaturePresent(config.Kubernetes) {
		return errors.New("the Agent is not running in Kubernetes")
	}

	var err error

	c.store = store
	c.lastExpire = time.Now()
	c.expireFreq = expireFreq
	c.watcher, err = kubelet.NewPodWatcher(expireFreq, true)
	if err != nil {
		return err
	}

	return nil
}

func (c *collector) Pull(ctx context.Context) error {
	updatedPods, err := c.watcher.PullChanges(ctx)
	if err != nil {
		return err
	}

	events := c.parsePods(updatedPods)

	if time.Now().Sub(c.lastExpire) >= c.expireFreq {
		var expiredIDs []string
		expiredIDs, err = c.watcher.Expire()
		if err == nil {
			events = append(events, c.parseExpires(expiredIDs)...)
			c.lastExpire = time.Now()
		}
	}

	c.store.Notify(events)

	return err
}

func (c *collector) parsePods(pods []*kubelet.Pod) []workloadmeta.Event {
	events := []workloadmeta.Event{}

	for _, pod := range pods {
		podMeta := pod.Metadata
		if podMeta.UID == "" {
			log.Debugf("pod has no UID. meta: %+v", podMeta)
			continue
		}

		containerSpecs := make(
			[]kubelet.ContainerSpec, 0,
			len(pod.Spec.Containers)+len(pod.Spec.InitContainers),
		)
		containerSpecs = append(containerSpecs, pod.Spec.InitContainers...)
		containerSpecs = append(containerSpecs, pod.Spec.Containers...)

		containerIDs, containerEvents := c.parsePodContainers(
			containerSpecs,
			pod.Status.GetAllContainers(),
		)

		podOwners := pod.Owners()
		owners := make([]workloadmeta.KubernetesPodOwner, 0, len(podOwners))
		for _, o := range podOwners {
			owners = append(owners, workloadmeta.KubernetesPodOwner{
				Kind: o.Kind,
				Name: o.Name,
				ID:   o.ID,
			})
		}

		entity := workloadmeta.KubernetesPod{
			EntityID: workloadmeta.EntityID{
				Kind: workloadmeta.KindKubernetesPod,
				ID:   podMeta.UID,
			},
			EntityMeta: workloadmeta.EntityMeta{
				Name:        podMeta.Name,
				Namespace:   podMeta.Namespace,
				Annotations: podMeta.Annotations,
				Labels:      podMeta.Labels,
			},
			Owners:                     owners,
			PersistentVolumeClaimNames: pod.GetPersistentVolumeClaimNames(),
			Containers:                 containerIDs,
			Ready:                      kubelet.IsPodReady(pod),
			Phase:                      pod.Status.Phase,
			IP:                         pod.Status.PodIP,
			PriorityClass:              pod.Spec.PriorityClassName,
		}

		events = append(events, containerEvents...)
		events = append(events, workloadmeta.Event{
			Source: collectorID,
			Type:   workloadmeta.EventTypeSet,
			Entity: entity,
		})
	}

	return events
}

func (c *collector) parsePodContainers(
	containerSpecs []kubelet.ContainerSpec,
	containerStatuses []kubelet.ContainerStatus,
) ([]string, []workloadmeta.Event) {
	containerIDs := make([]string, 0, len(containerStatuses))
	events := make([]workloadmeta.Event, 0, len(containerStatuses))

	for _, container := range containerStatuses {
		if container.ID == "" {
			// A container without an ID has not been created by
			// the runtime yet, so we ignore them until it's
			// detected again.
			continue
		}

		var env map[string]string
		var image workloadmeta.ContainerImage
		var ports []workloadmeta.ContainerPort

		runtime, containerID := containers.SplitEntityName(container.ID)
		containerIDs = append(containerIDs, containerID)

		containerSpec := findContainerSpec(container.Name, containerSpecs)
		if containerSpec != nil {
			env = extractEnvFromSpec(containerSpec.Env)
			image = buildImage(containerSpec.Image)

			ports = make([]workloadmeta.ContainerPort, 0, len(containerSpec.Ports))
			for _, port := range containerSpec.Ports {
				ports = append(ports, workloadmeta.ContainerPort{
					Name: port.Name,
					Port: port.ContainerPort,
				})
			}
		} else {
			log.Debugf("cannot find spec for container %q", container.Name)
		}

		image.ID = container.ImageID

		containerState := workloadmeta.ContainerState{}
		if st := container.State.Running; st != nil {
			containerState.Running = true
			containerState.StartedAt = st.StartedAt
		} else if st := container.State.Terminated; st != nil {
			containerState.Running = false
			containerState.StartedAt = st.StartedAt
			containerState.FinishedAt = st.FinishedAt
		}

		events = append(events, workloadmeta.Event{
			Source: collectorID,
			Type:   workloadmeta.EventTypeSet,
			Entity: workloadmeta.Container{
				EntityID: workloadmeta.EntityID{
					Kind: workloadmeta.KindContainer,
					ID:   containerID,
				},
				EntityMeta: workloadmeta.EntityMeta{
					Name: container.Name,
				},
				Image:   image,
				EnvVars: env,
				Ports:   ports,
				Runtime: workloadmeta.ContainerRuntime(runtime),
				State:   containerState,
			},
		})
	}

	return containerIDs, events
}

func findContainerSpec(name string, specs []kubelet.ContainerSpec) *kubelet.ContainerSpec {
	for _, spec := range specs {
		if spec.Name == name {
			return &spec
		}
	}

	return nil
}

func extractEnvFromSpec(envSpec []kubelet.EnvVar) map[string]string {
	env := make(map[string]string)
	mappingFunc := expansion.MappingFuncFor(env)

	// TODO: Implement support of environment variables set from ConfigMap,
	// Secret, DownwardAPI.
	// See https://github.com/kubernetes/kubernetes/blob/d20fd4088476ec39c5ae2151b8fffaf0f4834418/pkg/kubelet/kubelet_pods.go#L566
	// for the complete environment variable resolution process that is
	// done by the kubelet.

	for _, e := range envSpec {
		runtimeVal := e.Value
		if runtimeVal != "" {
			runtimeVal = expansion.Expand(runtimeVal, mappingFunc)
		}

		env[e.Name] = runtimeVal
	}

	return env
}

func buildImage(imageSpec string) workloadmeta.ContainerImage {
	image := workloadmeta.ContainerImage{
		RawName: imageSpec,
		Name:    imageSpec,
	}

	name, shortName, tag, err := containers.SplitImageName(imageSpec)
	if err != nil {
		log.Debugf("cannot split image name %q: %s", imageSpec, err)
		return image
	}

	if tag == "" {
		// k8s defaults to latest if tag is omitted
		tag = "latest"
	}

	image.Name = name
	image.ShortName = shortName
	image.Tag = tag

	return image
}

func (c *collector) parseExpires(expiredIDs []string) []workloadmeta.Event {
	events := make([]workloadmeta.Event, 0, len(expiredIDs))

	for _, expiredID := range expiredIDs {
		prefix, id := containers.SplitEntityName(expiredID)

		var kind workloadmeta.Kind
		if prefix == kubelet.KubePodEntityName {
			kind = workloadmeta.KindKubernetesPod
		} else {
			kind = workloadmeta.KindContainer
		}

		events = append(events, workloadmeta.Event{
			Source: collectorID,
			Type:   workloadmeta.EventTypeUnset,
			Entity: workloadmeta.EntityID{
				Kind: kind,
				ID:   id,
			},
		})
	}

	return events
}
