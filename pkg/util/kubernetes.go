package util

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/appscode/kutil"
	core_util "github.com/appscode/kutil/core/v1"
	api "github.com/appscode/stash/apis/stash/v1alpha1"
	stash_listers "github.com/appscode/stash/listers/stash/v1alpha1"
	"github.com/appscode/stash/pkg/docker"
	"github.com/cenkalti/backoff"
	"github.com/google/go-cmp/cmp"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

const (
	StashContainer = "stash"

	LocalVolumeName      = "stash-local"
	ScratchDirVolumeName = "stash-scratchdir"
	PodinfoVolumeName    = "stash-podinfo"
)

func GetAppliedRestic(m map[string]string) (*api.Restic, error) {
	data := GetString(m, api.LastAppliedConfiguration)
	if data == "" {
		return nil, nil
	}
	obj, err := kutil.UnmarshalToJSON([]byte(data), api.SchemeGroupVersion)
	if err != nil {
		return nil, err
	}
	restic, ok := obj.(*api.Restic)
	if !ok {
		return nil, fmt.Errorf("%s annotations has invalid Rectic object", api.LastAppliedConfiguration)
	}
	return restic, nil
}

func FindRestic(lister stash_listers.ResticLister, obj metav1.ObjectMeta) (*api.Restic, error) {
	restics, err := lister.Restics(obj.Namespace).List(labels.Everything())
	if kerr.IsNotFound(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	result := make([]*api.Restic, 0)
	for _, restic := range restics {
		selector, err := metav1.LabelSelectorAsSelector(&restic.Spec.Selector)
		if err != nil {
			return nil, err
		}
		if selector.Matches(labels.Set(obj.Labels)) {
			result = append(result, restic)
		}
	}
	if len(result) > 1 {
		var msg bytes.Buffer
		msg.WriteString(fmt.Sprintf("Workload %s/%s matches multiple Restics:", obj.Namespace, obj.Name))
		for i, restic := range result {
			if i > 0 {
				msg.WriteString(", ")
			}
			msg.WriteString(restic.Name)
		}
		return nil, errors.New(msg.String())
	} else if len(result) == 1 {
		return result[0], nil
	}
	return nil, nil
}

func WaitUntilSidecarAdded(kubeClient kubernetes.Interface, namespace string, selector *metav1.LabelSelector) error {
	return backoff.Retry(func() error {
		r, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return err
		}
		pods, err := kubeClient.CoreV1().Pods(namespace).List(metav1.ListOptions{LabelSelector: r.String()})
		if err != nil {
			return err
		}

		var podsToRestart []core.Pod
		for _, pod := range pods.Items {
			found := false
			for _, c := range pod.Spec.Containers {
				if c.Name == StashContainer {
					found = true
					break
				}
			}
			if !found {
				podsToRestart = append(podsToRestart, pod)
			}
		}
		if len(podsToRestart) == 0 {
			return nil
		}
		for _, pod := range podsToRestart {
			kubeClient.CoreV1().Pods(namespace).Delete(pod.Name, &metav1.DeleteOptions{})
		}
		return errors.New("check again")
	}, backoff.NewConstantBackOff(3*time.Second))
}

func WaitUntilSidecarRemoved(kubeClient kubernetes.Interface, namespace string, selector *metav1.LabelSelector) error {
	return backoff.Retry(func() error {
		r, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return err
		}
		pods, err := kubeClient.CoreV1().Pods(namespace).List(metav1.ListOptions{LabelSelector: r.String()})
		if err != nil {
			return err
		}

		var podsToRestart []core.Pod
		for _, pod := range pods.Items {
			found := false
			for _, c := range pod.Spec.Containers {
				if c.Name == StashContainer {
					found = true
					break
				}
			}
			if found {
				podsToRestart = append(podsToRestart, pod)
			}
		}
		if len(podsToRestart) == 0 {
			return nil
		}
		for _, pod := range podsToRestart {
			kubeClient.CoreV1().Pods(namespace).Delete(pod.Name, &metav1.DeleteOptions{})
		}
		return errors.New("check again")
	}, backoff.NewConstantBackOff(3*time.Second))
}

func GetString(m map[string]string, key string) string {
	if m == nil {
		return ""
	}
	return m[key]
}

func CreateSidecarContainer(r *api.Restic, tag, workload string) core.Container {
	if r.Annotations != nil {
		if v, ok := r.Annotations[api.VersionTag]; ok {
			tag = v
		}
	}
	sidecar := core.Container{
		Name:            StashContainer,
		Image:           docker.ImageOperator + ":" + tag,
		ImagePullPolicy: core.PullIfNotPresent,
		Args: []string{
			"schedule",
			"--restic-name=" + r.Name,
			"--workload=" + workload,
		},
		Env: []core.EnvVar{
			{
				Name: "NODE_NAME",
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{
						FieldPath: "spec.nodeName",
					},
				},
			},
			{
				Name: "POD_NAME",
				ValueFrom: &core.EnvVarSource{
					FieldRef: &core.ObjectFieldSelector{
						FieldPath: "metadata.name",
					},
				},
			},
		},
		Resources: r.Spec.Resources,
		VolumeMounts: []core.VolumeMount{
			{
				Name:      ScratchDirVolumeName,
				MountPath: "/tmp",
			},
			{
				Name:      PodinfoVolumeName,
				MountPath: "/etc/stash",
			},
		},
	}
	if tag == "canary" {
		sidecar.ImagePullPolicy = core.PullAlways
		sidecar.Args = append(sidecar.Args, "--v=5")
	} else {
		sidecar.Args = append(sidecar.Args, "--v=3")
	}
	for _, srcVol := range r.Spec.VolumeMounts {
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, core.VolumeMount{
			Name:      srcVol.Name,
			MountPath: srcVol.MountPath,
			SubPath:   srcVol.SubPath,
			ReadOnly:  true,
		})
	}
	if r.Spec.Backend.Local != nil {
		sidecar.VolumeMounts = append(sidecar.VolumeMounts, core.VolumeMount{
			Name:      LocalVolumeName,
			MountPath: r.Spec.Backend.Local.Path,
		})
	}
	return sidecar
}

func UpsertScratchVolume(volumes []core.Volume) []core.Volume {
	return core_util.UpsertVolume(volumes, core.Volume{
		Name: ScratchDirVolumeName,
		VolumeSource: core.VolumeSource{
			EmptyDir: &core.EmptyDirVolumeSource{},
		},
	})
}

// https://kubernetes.io/docs/tasks/inject-data-application/downward-api-volume-expose-pod-information/#store-pod-fields
func UpsertDownwardVolume(volumes []core.Volume) []core.Volume {
	return core_util.UpsertVolume(volumes, core.Volume{
		Name: PodinfoVolumeName,
		VolumeSource: core.VolumeSource{
			DownwardAPI: &core.DownwardAPIVolumeSource{
				Items: []core.DownwardAPIVolumeFile{
					{
						Path: "labels",
						FieldRef: &core.ObjectFieldSelector{
							FieldPath: "metadata.labels",
						},
					},
				},
			},
		},
	})
}

func MergeLocalVolume(volumes []core.Volume, old, new *api.Restic) []core.Volume {
	oldPos := -1
	if old != nil && old.Spec.Backend.Local != nil {
		for i, vol := range volumes {
			if vol.Name == LocalVolumeName {
				oldPos = i
				break
			}
		}
	}
	if new.Spec.Backend.Local != nil {
		if oldPos != -1 {
			volumes[oldPos] = core.Volume{Name: LocalVolumeName, VolumeSource: new.Spec.Backend.Local.VolumeSource}
		} else {
			volumes = core_util.UpsertVolume(volumes, core.Volume{Name: LocalVolumeName, VolumeSource: new.Spec.Backend.Local.VolumeSource})
		}
	} else {
		if oldPos != -1 {
			volumes = append(volumes[:oldPos], volumes[oldPos+1:]...)
		}
	}
	return volumes
}

func EnsureVolumeDeleted(volumes []core.Volume, name string) []core.Volume {
	for i, v := range volumes {
		if v.Name == name {
			return append(volumes[:i], volumes[i+1:]...)
		}
	}
	return volumes
}

func ResticEqual(old, new *api.Restic) bool {
	var oldSpec, newSpec *api.ResticSpec
	if old != nil {
		oldSpec = &old.Spec
	}
	if new != nil {
		newSpec = &new.Spec
	}
	return cmp.Equal(oldSpec, newSpec, cmp.Comparer(func(x, y resource.Quantity) bool {
		return x.Cmp(y) == 0
	}))
}

func RecoveryEqual(old, new *api.Recovery) bool {
	var oldSpec, newSpec *api.RecoverySpec
	if old != nil {
		oldSpec = &old.Spec
	}
	if new != nil {
		newSpec = &new.Spec
	}
	return reflect.DeepEqual(oldSpec, newSpec)
}
